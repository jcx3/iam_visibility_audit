// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Command iam_visibility_audit will enumerate all projects and organizations Workspace users may have access to outside
of their primary organizationID.

see:
 https://cloud.google.com/resource-manager/docs/access-control-org#restricting_visibility

Arguments:

	impersonatedServiceAccount = flag.String("impersonatedServiceAccount", "", "Impersonated Service Accounts the script should run as")
	organization               = flag.String("organization", "", "The organizationID that is the subject of this audit")
	subject                    = flag.String("subject", "", "The admin user to for the organization that can use the Directory API to list users")
	cx                         = flag.String("cx", "", "Workspace Customer ID number")
	serviceAccountFile         = flag.String("serviceAccountFile", "", "Servie Account JSON files with IAM permissions to the org")

	-v 10  adjust log verbosity level

Usage:

$ go run main.go --impersonatedServiceAccount=dwd-sa@$PROJECT_ID.iam.gserviceaccount.com \
  --subject=$DOMAIN_ADMIN \
  --organization $ORGANIZATION_ID \
  -cx $CX --alsologtostderr=1 -v 10
*/
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"strings"
	"sync"
	"time"

	asset "cloud.google.com/go/asset/apiv1"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	"golang.org/x/time/rate"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	assetpb "google.golang.org/genproto/googleapis/cloud/asset/v1"
)

var ()

const (
	maxRequestsPerSecond  float64 = 4 // "golang.org/x/time/rate" limiter to throttle operations
	burst                 int     = 1
	maxPageSize           int64   = 1000
	assetTypeOrganization string  = "cloudresourcemanager.googleapis.com/Organization"
	assetTypeProject      string  = "cloudresourcemanager.googleapis.com/Project"
)

type userAccess struct {
	User          admin.User
	Organizations []*cloudresourcemanager.Organization
	Projects      []*cloudresourcemanager.Project
	Error         error
}

func main() {

	var wg sync.WaitGroup
	serviceAccountFile := flag.String("serviceAccountFile", "", "Service Account JSON files with IAM permissions to the org")
	impersonatedServiceAccount := flag.String("impersonatedServiceAccount", "", "Impersonated Service Accounts the script should run as")
	organization := flag.String("organization", "", "The organizationID that is the subject of this audit")
	subject := flag.String("subject", "", "The admin user to for the organization that can use the Directory API to list users")
	cx := flag.String("cx", "", "Workspace Customer ID number")
	delay := flag.Int("delay", 1*1000, "delay in ms for each user iterated")

	flag.Parse()
	defer glog.Flush()
	ctx := context.Background()

	// Configure a rate limiter that will control how frequently API calls to
	// Cloud Resource Manager is made.
	limiter := rate.NewLimiter(rate.Limit(maxRequestsPerSecond), burst)

	// Initialize a randomSeed for use later
	rand.Seed(time.Now().UnixNano())

	if *organization == "" || *subject == "" || *cx == "" {
		glog.Error("--organization, --cx and --subject must be specified")
		return
	}
	if (*serviceAccountFile == "" && *impersonatedServiceAccount == "") || (*serviceAccountFile != "" && *impersonatedServiceAccount != "") {
		glog.Error("either --serviceAccountFile or --impersonatedServiceAccount must be specified")
		return
	}

	// Parse the serviceAccount names or keys provided. Multiple values can be set to shard API quota between projects.
	serviceAccounts, svcAccountKeys, err := parseServiceAccounts(*impersonatedServiceAccount, *serviceAccountFile)
	if err != nil {
		glog.Errorf("Error parsing serviceAccounts %v", err)
		return
	}

	// Select an random serviceAccount to use.
	impersonateAccount, svcKeyCred := getRandomServiceAccount(serviceAccounts, svcAccountKeys)

	// Initialize the Cloud Asset API.
	// This api is used to find the projects in an ORG
	assetClient, err := getAssetClient(ctx, impersonateAccount, svcKeyCred)
	if err != nil {
		glog.Errorf("Error getting Cloud Asset API Client %v", err)
		return
	}

	// Use the Cloud Asset API to recall/find the Organization for provided organizationID.
	// We do not really need to do this section since the the orgID was provided in the input argument.
	// We are also setting a default "emptyQuery" value here to return all projects.
	// We are doing this as a type of input validation with the asset-api to search/narrow the organization.
	emptyQuery := ""
	allOrganizations, err := findResourcesByAssetType(ctx, *organization, assetTypeOrganization, emptyQuery, assetClient)
	if err != nil {
		glog.Errorf("Error finding Organizations %v", err)
		return
	}

	// Use the Cloud Asset API to recall/find the projects for provided organizationID.
	// We do not use the Resource Manager api here since that would return all the projects
	// the caller has access to, not just those restricted to the organization we want.
	// We are also setting a default "emptyQuery" value here to return all projects.
	// You can specify a query filter here to configure the set of projects to evaluate.
	// see https://cloud.google.com/asset-inventory/docs/searching-resources#how_to_construct_a_query
	allProjects, err := findResourcesByAssetType(ctx, *organization, assetTypeProject, emptyQuery, assetClient)
	if err != nil {
		glog.Errorf("Error finding all projects in the organization %v", err)
		return
	}

	glog.V(20).Infoln("      Getting Users")

	// Initialize the Workspace Admin client
	// This client will be used to find users in a given Cloud Identity/Workspace domain
	adminService, err := getAdminServiceClient(ctx, impersonateAccount, svcKeyCred, *subject)
	if err != nil {
		glog.Errorf("Error initializing admin client %v", err)
		return
	}

	// If you want to narrow the search to a subset of users, apply a searchFilter here
	//  https://developers.google.com/admin-sdk/directory/v1/guides/search-users
	//	eg. searchFilter := "isAdmin=false"

	searchFilter := ""
	allUsers, err := findDomainUsers(ctx, *cx, searchFilter, adminService)
	if err != nil {
		glog.Errorf("Error finding domain users %v", err)
		return
	}

	// Launch a goroutines that will process the projects and organizations
	// that are visible to users.  The channel is fed by the
	// getOrganizations(), and getProjects() function calls in the succeeding stanza.
	ch := make(chan *userAccess)
	done := make(chan bool)
	go func() {
		for {
			msg, ok := <-ch
			if ok {
				if msg.Error != nil {
					glog.Errorf("Error iterating: %v", msg.Error)
				} else {
					for _, o := range msg.Organizations {
						glog.V(50).Infof("             User %s has Organization visibility to %s", msg.User.PrimaryEmail, o.Name)
						if _, ok := allOrganizations[o.Name]; !ok {
							glog.V(2).Infof("             User [%s] has external organization visibility to [%s](%s)", msg.User.PrimaryEmail, o.Name, o.DisplayName)
						}
					}
					for _, p := range msg.Projects {
						glog.V(50).Infof("             User %s has Project visibility to %s", msg.User.PrimaryEmail, p.ProjectId)
						if _, ok := allProjects[p.ProjectId]; !ok {
							glog.V(2).Infof("             User [%s] has external project visibility to [projects/%d](%s)", msg.User.PrimaryEmail, p.ProjectNumber, p.ProjectId)
						}
					}
				}
			} else {
				done <- true
				return
			}
		}
	}()

	// Launch goroutines for each user that feeds a channel received the the go routine defined in the preceding stanza.
	for _, u := range allUsers {

		// Select a random service account to use per user and disribute quota consumption
		impersonateAccount, svcKeyCred := getRandomServiceAccount(serviceAccounts, svcAccountKeys)

		// We need to iterate over all organizations visible to the current user
		// The noFilter is explicitly defined here to allow you to modify which organizations to
		// limit the query.
		// see https://pkg.go.dev/google.golang.org/api@v0.58.0/cloudresourcemanager/v1#SearchOrganizationsRequest
		noFilter := ""
		wg.Add(1)
		go getOrganizations(ctx, ch, &wg, limiter, noFilter, impersonateAccount, svcKeyCred, *u)

		//  The Project.List() accepts a Filter parameter which will return a subset
		//  of projects that match the  specifications.
		//  By default, if the Filter value is not set, all projects will be returned.
		//  However, the code below leaves the parameter
		//  explicitly defined in the event you want to query and evaluate on a
		//  subset of projects.  See:
		//  https://pkg.go.dev/google.golang.org/api@v0.58.0/cloudresourcemanager/v1#ProjectsListCall.Filter
		wg.Add(1)
		go getProjects(ctx, ch, &wg, limiter, noFilter, impersonateAccount, svcKeyCred, *u)
		time.Sleep(time.Duration(*delay) * time.Millisecond)
	}

	wg.Wait()
	// all messages should be in the channel now, close it
	close(ch)
	// wait for all the messages to get processed
	<-done
}

// If multiple service accounts are specified in the command line, parse each one of them
func parseServiceAccounts(impersonatedAccounts, keysFiles string) ([]string, [][]byte, error) {
	var serviceAccountPool []string
	var keyBytesPool [][]byte
	if keysFiles != "" {
		for _, k := range strings.Split(keysFiles, ",") {
			svcAccountJSONBytes, err := ioutil.ReadFile(k)
			if err != nil {
				return nil, nil, err
			}
			keyBytesPool = append(keyBytesPool, svcAccountJSONBytes)
		}
	}

	if impersonatedAccounts != "" {
		serviceAccountPool = strings.Split(impersonatedAccounts, ",")
	}

	return serviceAccountPool, keyBytesPool, nil
}

func getRandomServiceAccount(accounts []string, keys [][]byte) (string, []byte) {
	var selectedAccount string
	var selectedKey []byte
	if len(accounts) > 0 {
		selectedAccount = accounts[rand.Intn(len(accounts))]
		glog.V(50).Infof("             Selecting serviceAccount: %s", selectedAccount)
	}
	if len(keys) > 0 {
		keyIndex := rand.Intn(len(keys))
		selectedKey = keys[keyIndex]
		glog.V(50).Infof("             Selecting serviceAccount keyIndex value: %d", keyIndex)
	}
	return selectedAccount, selectedKey
}

func findDomainUsers(ctx context.Context, cx string, searchFilter string, adminService *admin.Service) ([]*admin.User, error) {

	allUsers := make([]*admin.User, 0)
	pageToken := ""
	q := adminService.Users.List().Customer(cx).Query(searchFilter)
	for {
		if pageToken != "" {
			q = q.PageToken(pageToken)
		}
		r, err := q.Do()
		if err != nil {
			return nil, err
		}
		for _, u := range r.Users {
			glog.V(20).Infof("      Found User: %s", u.PrimaryEmail)
			allUsers = append(allUsers, u)
		}
		pageToken = r.NextPageToken
		if pageToken == "" {
			break
		}
	}
	return allUsers, nil
}

func findResourcesByAssetType(ctx context.Context, organizationID string, assetType string, query string, assetClient *asset.Client) (map[string]*assetpb.ResourceSearchResult, error) {

	resourceList := make(map[string]*assetpb.ResourceSearchResult)

	req := &assetpb.SearchAllResourcesRequest{
		Scope:      fmt.Sprintf("organizations/%s", organizationID),
		Query:      query,
		AssetTypes: []string{assetType},
	}

	it := assetClient.SearchAllResources(ctx, req)
	for {
		response, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}

		switch {
		case assetType == assetTypeOrganization:
			orgName := strings.TrimPrefix(response.Name, "//cloudresourcemanager.googleapis.com/organizations/")
			glog.V(20).Infof("     Found Organization %s", orgName)
			resourceList[fmt.Sprintf("organizations/%s", orgName)] = response
		case assetType == assetTypeProject:
			projectID := strings.TrimPrefix(response.Name, "//cloudresourcemanager.googleapis.com/projects/")
			glog.V(20).Infof("     Found projectID %s", projectID)
			resourceList[projectID] = response
		default:
			return nil, fmt.Errorf(fmt.Sprintf("Error getting resources:  unknown assetType: %s", assetType))
		}
	}
	return resourceList, nil
}

func getAssetClient(ctx context.Context, impersonateAccount string, serviceAccountData []byte) (*asset.Client, error) {
	if impersonateAccount != "" {
		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: impersonateAccount,
			Scopes:          []string{cloudresourcemanager.CloudPlatformScope},
		})
		if err != nil {
			return nil, err
		}
		return asset.NewClient(ctx, option.WithTokenSource(ts))

	} else {
		cred, err := google.CredentialsFromJSONWithParams(ctx, serviceAccountData, google.CredentialsParams{
			Scopes: []string{cloudresourcemanager.CloudPlatformScope},
		})
		if err != nil {
			return nil, err
		}
		return asset.NewClient(ctx, option.WithCredentials(cred))
	}
}

func getAdminServiceClient(ctx context.Context, impersonateAccount string, serviceAccountData []byte, impersonatedUser string) (*admin.Service, error) {
	if impersonateAccount != "" {
		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: impersonateAccount,
			Scopes:          []string{admin.AdminDirectoryUserReadonlyScope},
			Subject:         impersonatedUser,
		})
		if err != nil {
			return nil, err
		}
		return admin.NewService(ctx, option.WithTokenSource(ts))
	} else {
		cred, err := google.CredentialsFromJSONWithParams(ctx, serviceAccountData, google.CredentialsParams{
			Scopes:  []string{admin.AdminDirectoryUserReadonlyScope},
			Subject: impersonatedUser,
		})
		if err != nil {
			return nil, err
		}
		return admin.NewService(ctx, option.WithCredentials(cred))
	}
}

func getResourceManagerClient(ctx context.Context, impersonateAccount string, serviceAccountData []byte, impersonatedUser string) (*cloudresourcemanager.Service, error) {
	if impersonateAccount != "" {
		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: impersonateAccount,
			Scopes:          []string{cloudresourcemanager.CloudPlatformReadOnlyScope},
			Subject:         impersonatedUser,
		})
		if err != nil {
			return nil, err
		}
		return cloudresourcemanager.NewService(ctx, option.WithTokenSource(ts))
	} else {
		cred, err := google.CredentialsFromJSONWithParams(ctx, serviceAccountData, google.CredentialsParams{
			Scopes:  []string{cloudresourcemanager.CloudPlatformReadOnlyScope},
			Subject: impersonatedUser,
		})
		if err != nil {
			return nil, err
		}
		return cloudresourcemanager.NewService(ctx, option.WithCredentials(cred))
	}
}

func getOrganizations(ctx context.Context, ch chan<- *userAccess, wg *sync.WaitGroup, limiter *rate.Limiter, filter string, impersonateAccount string, serviceAccountData []byte, u admin.User) {
	glog.V(50).Infof("             Getting Organizations for user %s", u.PrimaryEmail)
	defer wg.Done()

	crmService, err := getResourceManagerClient(ctx, impersonateAccount, serviceAccountData, u.PrimaryEmail)
	if err != nil {
		ch <- &userAccess{
			User:  u,
			Error: fmt.Errorf("error getting Cloud ResourceManager client for organizations for user %s %v", u.PrimaryEmail, err),
		}
		return
	}
	organizations := make([]*cloudresourcemanager.Organization, 0)

	req := crmService.Organizations.Search(&cloudresourcemanager.SearchOrganizationsRequest{Filter: filter, PageSize: maxPageSize})

	err = req.Pages(ctx, func(page *cloudresourcemanager.SearchOrganizationsResponse) error {
		organizations = append(organizations, page.Organizations...)
		if err := limiter.Wait(ctx); err != nil {
			glog.Errorf("Error in rate limiter for user %s %v", u.PrimaryEmail, err)
			return err
		}
		return nil
	})
	if err != nil {
		ch <- &userAccess{
			User:  u,
			Error: fmt.Errorf("error iterating visible organizations for user %s %v", u.PrimaryEmail, err),
		}
		return
	}

	ch <- &userAccess{
		User:          u,
		Organizations: organizations,
	}
}

func getProjects(ctx context.Context, ch chan<- *userAccess, wg *sync.WaitGroup, limiter *rate.Limiter, filter string, impersonateAccount string, serviceAccountData []byte, u admin.User) {
	glog.V(50).Infof("             Getting Projects for user %s", u.PrimaryEmail)
	defer wg.Done()

	crmService, err := getResourceManagerClient(ctx, impersonateAccount, serviceAccountData, u.PrimaryEmail)
	if err != nil {
		ch <- &userAccess{
			User:  u,
			Error: fmt.Errorf("error getting Cloud ResourceManager client for projects for user %s %v", u.PrimaryEmail, err),
		}
		return
	}

	projects := make([]*cloudresourcemanager.Project, 0)
	req := crmService.Projects.List().Filter(filter).PageSize(maxPageSize)
	err = req.Pages(ctx, func(page *cloudresourcemanager.ListProjectsResponse) error {
		projects = append(projects, page.Projects...)
		if err := limiter.Wait(ctx); err != nil {
			glog.Errorf("Error in rate limiter for user %s %v", u.PrimaryEmail, err)
			return err
		}
		return nil
	})
	if err != nil {
		ch <- &userAccess{
			User:  u,
			Error: fmt.Errorf("error iterating visible projects for user %s %v", u.PrimaryEmail, err),
		}
		return
	}

	ch <- &userAccess{
		User:     u,
		Projects: projects,
	}
}
