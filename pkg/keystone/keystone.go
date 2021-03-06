/*******************************************************************************
*
* Copyright 2017 SAP SE
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You should have received a copy of the License along with this
* program. If not, you may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*
*******************************************************************************/

package keystone

import (
	"fmt"

	"net/http"
	"net/url"
	"sync"

	"github.com/databus23/goslo.policy"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/roles"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/users"
	"github.com/gophercloud/gophercloud/pagination"
	"github.com/patrickmn/go-cache"
	"github.com/sapcc/maia/pkg/util"
	"github.com/spf13/viper"
	"math"
	"math/rand"
	"strings"
	"time"
)

// Keystone creates a real keystone authentication and authorization driver
func Keystone() Driver {
	ks := keystone{}
	ks.init()

	return &ks
}

type keystone struct {
	// these locks are used to make sure the connection or token is altered while somebody is working on it
	serviceConnMutex, serviceTokenMutex *sync.Mutex
	// these caches are thread-safe, no need to lock because worst-case is duplicate processing efforts
	tokenCache, projectTreeCache, userProjectsCache, userIDCache *cache.Cache
	providerClient                                               *gophercloud.ServiceClient
	seqErrors                                                    int
	serviceURL                                                   string
	// role-id --> role-name
	monitoringRoles map[string]string
	// domain-id --> domain-name
	domainNames map[string]string
	// domain-name --> domain-id
	domainIDs map[string]string
}

func (d *keystone) init() {
	d.tokenCache = cache.New(viper.GetDuration("keystone.token_cache_time"), time.Minute)
	d.projectTreeCache = cache.New(viper.GetDuration("keystone.token_cache_time"), time.Minute)
	d.userProjectsCache = cache.New(viper.GetDuration("keystone.token_cache_time"), time.Minute)
	d.userIDCache = cache.New(cache.NoExpiration, time.Minute)
	d.serviceConnMutex = &sync.Mutex{}
	d.serviceTokenMutex = &sync.Mutex{}
	if viper.Get("keystone.username") != nil {
		// force service logon
		_, err := d.serviceKeystoneClient()
		if err != nil {
			panic(err)
		}
	}
}

func (d *keystone) serviceKeystoneClient() (*gophercloud.ServiceClient, error) {
	d.serviceConnMutex.Lock()
	defer d.serviceConnMutex.Unlock()

	if d.providerClient == nil {
		util.LogInfo("Setting up identity connection to %s", viper.GetString("keystone.auth_url"))
		client, err := newKeystoneClient()
		if err != nil {
			return nil, err
		}
		d.providerClient = client
		if err := d.reauthServiceUser(); err != nil {
			d.providerClient = nil
			return nil, err
		}
	}

	return d.providerClient, nil
}

func newKeystoneClient() (*gophercloud.ServiceClient, error) {
	provider, err := openstack.NewClient(viper.GetString("keystone.auth_url"))
	if viper.IsSet("maia.proxy") {
		proxyURL, err := url.Parse(viper.GetString("maia.proxy"))
		if err != nil {
			util.LogError("Could not set proxy for gophercloud client: %s .\n%s", proxyURL, err.Error())
			return nil, err
		}
		provider.HTTPClient.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}
	client, err := openstack.NewIdentityV3(provider, gophercloud.EndpointOpts{
		Region: "",
	})
	if err != nil {
		return nil, fmt.Errorf("cannot initialize OpenStack client: %v", err)
	}

	return client, nil
}

type keystoneToken struct {
	DomainScope  keystoneTokenThing         `json:"domain"`
	ProjectScope keystoneTokenThingInDomain `json:"project"`
	Roles        []keystoneTokenThing       `json:"roles"`
	User         keystoneTokenThingInDomain `json:"user"`
	Token        string
	ExpiresAt    string `json:"expires_at"`
}

type keystoneTokenThing struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type keystoneTokenThingInDomain struct {
	keystoneTokenThing
	Domain keystoneTokenThing `json:"domain"`
}

func (t *keystoneToken) ToContext() policy.Context {
	c := policy.Context{
		Roles: make([]string, 0, len(t.Roles)),
		Auth: map[string]string{
			"user_id":             t.User.ID,
			"user_name":           t.User.Name,
			"user_domain_id":      t.User.Domain.ID,
			"user_domain_name":    t.User.Domain.Name,
			"domain_id":           t.DomainScope.ID,
			"domain_name":         t.DomainScope.Name,
			"project_id":          t.ProjectScope.ID,
			"project_name":        t.ProjectScope.Name,
			"project_domain_id":   t.ProjectScope.Domain.ID,
			"project_domain_name": t.ProjectScope.Domain.Name,
			"token":               t.Token,
			"token-expiry":        t.ExpiresAt,
		},
		Request: map[string]string{
			"user_id":    t.User.ID,
			"domain_id":  t.DomainScope.ID,
			"project_id": t.ProjectScope.ID,
		},
		Logger: util.LogDebug,
	}
	for key, value := range c.Auth {
		if value == "" {
			delete(c.Auth, key)
		}
	}
	for _, role := range t.Roles {
		c.Roles = append(c.Roles, role.Name)
	}

	return c
}

type cacheEntry struct {
	context     *policy.Context
	endpointURL string
	projectTree []string
}

// ServiceURL returns the service's global catalog entry
// The result is empty when called from a client
func (d *keystone) ServiceURL() string {
	return d.serviceURL
}

// reauthServiceUser refreshes an expired keystone token
func (d *keystone) reauthServiceUser() error {
	d.serviceTokenMutex.Lock()
	defer d.serviceTokenMutex.Unlock()

	authOpts := authOptionsFromConfig()
	util.LogInfo("Fetching token for service user %s%s@%s%s", authOpts.UserID, authOpts.Username, authOpts.DomainID, authOpts.DomainName)

	result := tokens.Create(d.providerClient, authOpts)
	token, err := result.ExtractToken()

	if err != nil {
		// wait ~ (2^errors)/2, i.e. 0..1, 0..2, 0..4, ... increasing with every sequential error
		r := rand.Intn(int(math.Exp2(float64(d.seqErrors))))
		time.Sleep(time.Duration(r) * time.Second)
		d.seqErrors++
		// clear token
		viper.Set("keystone.token", "")
		return NewAuthenticationError(StatusNotAvailable, "Cannot obtain token: %v (%d sequential errors)", err, d.seqErrors)
	}
	// read service catalog
	catalog, err := result.ExtractServiceCatalog()

	if err != nil {
		return NewAuthenticationError(StatusNotAvailable, "cannot read service catalog: %v", err)
	}
	d.serviceURL, err = openstack.V3EndpointURL(catalog, gophercloud.EndpointOpts{Type: "metrics", Availability: gophercloud.AvailabilityPublic})

	// store token so that it is considered for next authentication attempt
	viper.Set("keystone.token", token.ID)
	d.providerClient.TokenID = token.ID
	d.providerClient.ReauthFunc = d.reauthServiceUser
	d.providerClient.EndpointLocator = func(opts gophercloud.EndpointOpts) (string, error) {
		return openstack.V3EndpointURL(catalog, opts)
	}

	// get role-names from config and find the corresponding role-IDs
	d.loadDomainsAndRoles()

	return nil
}

func (d *keystone) loadDomainsAndRoles() {
	// load all roles

	// check if roles list already initialized
	if len(d.monitoringRoles) > 0 {
		return
	}

	allRoles := struct {
		Roles []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"roles"`
	}{}

	u := d.providerClient.ServiceURL("roles")
	_, err := d.providerClient.Get(u, &allRoles, nil)
	if err != nil {
		panic(err)
	}

	// get list of all monitoring role names
	rolesNames := strings.Split(viper.GetString("keystone.roles"), ",")

	d.monitoringRoles = map[string]string{}
	// get all known roles and match them with our own list to get the ID
	for _, ar := range allRoles.Roles {
		for _, name := range rolesNames {
			if ar.Name == name {
				d.monitoringRoles[ar.ID] = name
			}
		}
	}

	// load domains
	d.domainNames = map[string]string{}
	d.domainIDs = map[string]string{}
	trueVal := true
	err = projects.List(d.providerClient, projects.ListOpts{IsDomain: &trueVal, Enabled: &trueVal}).EachPage(func(page pagination.Page) (bool, error) {
		domains, err := projects.ExtractProjects(page)
		if err != nil {
			panic(err)
		}
		for _, domain := range domains {
			d.domainNames[domain.ID] = domain.Name
			d.domainIDs[domain.Name] = domain.ID
		}
		return true, nil
	})
	if err != nil {
		panic(err)
	}
}

func authOptionsFromConfig() *tokens.AuthOptions {
	return &tokens.AuthOptions{
		IdentityEndpoint: viper.GetString("keystone.auth_url"),
		TokenID:          viper.GetString("keystone.token"),
		Username:         viper.GetString("keystone.username"),
		Password:         viper.GetString("keystone.password"),
		DomainName:       viper.GetString("keystone.user_domain_name"),
		AllowReauth:      true,
		Scope: tokens.Scope{
			ProjectName: viper.GetString("keystone.project_name"),
			DomainName:  viper.GetString("keystone.project_domain_name"),
		},
	}
}

func authOpts2StringKey(authOpts *tokens.AuthOptions) string {
	if authOpts.TokenID != "" {
		return authOpts.TokenID + authOpts.Scope.ProjectID + " " + authOpts.Scope.ProjectName + " " +
			authOpts.Scope.DomainID + " " + authOpts.Scope.DomainName
	}

	// build unique key by separating fields with blanks. Since blanks are not allowed in several of those
	// the result will be unique
	return authOpts.UserID + " " + authOpts.Username + " " + authOpts.Password + " " + authOpts.DomainID + " " +
		authOpts.DomainName + " " + authOpts.Scope.ProjectID + " " + authOpts.Scope.ProjectName + " " +
		authOpts.Scope.DomainID + " " + authOpts.Scope.DomainName
}

// Authenticate authenticates a non-service user using available authOptionsFromRequest (username+password or token)
// It returns the authorization context
func (d *keystone) Authenticate(authOpts *tokens.AuthOptions) (*policy.Context, string, AuthenticationError) {
	return d.authenticate(authOpts, false)
}

// AuthenticateRequest attempts to Authenticate a user using the request header contents
// The resulting policy context can be used to authorize the user
// If no supported authOptionsFromRequest could be found, the context is nil
// If the authOptionsFromRequest are invalid or the authentication provider has issues, an error is returned
// When guessScope is set to true, the method will try to find a suitible project when the scope is not defined (basic auth. only)
func (d *keystone) AuthenticateRequest(r *http.Request, guessScope bool) (*policy.Context, AuthenticationError) {
	authOpts, err := d.authOptionsFromRequest(r, guessScope)
	if err != nil {
		util.LogError(err.Error())
		return nil, err
	}

	// if the request does not have a keystone token, then a new token has to be requested on behalf of the client
	// this must not happen with the connection of the service otherwise wrong credentials will cause reauthentication
	// of the service user
	context, _, err := d.authenticate(authOpts, true)
	if err != nil {
		return nil, err
	}

	// write this to request header (compatible with databus23/keystone)
	r.Header.Set("X-User-Id", context.Auth["user_id"])
	r.Header.Set("X-User-Name", context.Auth["user_name"])
	r.Header.Set("X-User-Domain-Id", context.Auth["user_domain_id"])
	r.Header.Set("X-User-Domain-Name", context.Auth["user_domain_name"])
	if context.Auth["project_id"] != "" {
		r.Header.Set("X-Project-Id", context.Auth["project_id"])
		r.Header.Set("X-Project-Name", context.Auth["project_name"])
		r.Header.Set("X-Project-Domain-Id", context.Auth["project_domain_id"])
		r.Header.Set("X-Project-Domain-Name", context.Auth["project_domain_name"])
	} else {
		r.Header.Set("X-Domain-Id", context.Auth["domain_id"])
		r.Header.Set("X-Domain-Name", context.Auth["domain_name"])
	}
	for _, role := range context.Roles {
		r.Header.Add("X-Roles", role)
	}
	r.Header.Set("X-Auth-Token", context.Auth["token"])
	r.Header.Set("X-Auth-Token-Expiry", context.Auth["token-expiry"])

	return context, nil
}

// authOptionsFromRequest retrieves authOptionsFromRequest from http request and puts them into an AuthOptions structure
// It requires username to contain a qualified OpenStack username and project/domain scope information
// Format: <user>"|"<project> or <user>"|@"<domain>
// user/project can either be a unique OpenStack ID or a qualified name with domain information, e.g. username"@"domain
// When guessScope is set to true, the method will try to find a suitible project when the scope is not defined (basic auth. only)
func (d *keystone) authOptionsFromRequest(r *http.Request, guessScope bool) (*tokens.AuthOptions, AuthenticationError) {
	ba := tokens.AuthOptions{
		IdentityEndpoint: viper.GetString("keystone.auth_url"),
		AllowReauth:      false,
	}

	// extract credentials
	if token := r.Header.Get("X-Auth-Token"); token != "" {
		ba.TokenID = token
	} else if username, password, ok := r.BasicAuth(); ok {
		usernameParts := strings.Split(username, "|")
		userParts := strings.Split(usernameParts[0], "@")
		var scopeParts []string
		if len(usernameParts) >= 2 {
			scopeParts = strings.Split(usernameParts[1], "@")
		} else {
			// default to arbitrary project with sufficient roles after knowing the user
			scopeParts = []string{}
		}

		// parse username part
		if len(userParts) > 1 {
			// username + user-domain-name
			ba.Username = userParts[0]
			ba.DomainName = userParts[1]
		} else if headerUserDomain := r.Header.Get("X-User-Domain-Name"); headerUserDomain != "" {
			ba.Username = userParts[0]
			ba.DomainName = headerUserDomain
		} else {
			ba.UserID = userParts[0]
		}

		// parse scope part
		if len(scopeParts) >= 2 {
			// assume domains are always prefixed with @
			if scopeParts[0] != "" {
				ba.Scope.ProjectName = scopeParts[0]
			}
			ba.Scope.DomainName = scopeParts[1]
		} else if len(scopeParts) >= 1 {
			ba.Scope.ProjectID = scopeParts[0]
		} else if guessScope {
			if err := d.guessScope(&ba); err != nil {
				return nil, err
			}
		}

		// set password
		ba.Password = password
	} else {
		return nil, NewAuthenticationError(StatusMissingCredentials, "Authorization header missing (no username/password or token)")
	}

	// check overriding project/domain via ULR param
	query := r.URL.Query()
	if projectID := query.Get("project_id"); projectID != "" {
		ba.Scope.ProjectID = projectID
		ba.Scope.ProjectName = ""
		ba.Scope.DomainID = ""
		ba.Scope.DomainName = ""
		query.Del("project_id")
	} else if domainID := query.Get("domain_id"); domainID != "" {
		ba.Scope.DomainID = domainID
		ba.Scope.DomainName = ""
		ba.Scope.ProjectName = ""
		ba.Scope.ProjectID = ""
		query.Del("domain_id")
	}

	return &ba, nil
}

func (d *keystone) guessScope(ba *tokens.AuthOptions) AuthenticationError {
	// guess scope if it is missing
	userID := ba.UserID
	var err error
	if userID == "" {
		userID, err = d.UserID(ba.Username, ba.DomainName)
		if err != nil {
			return NewAuthenticationError(StatusWrongCredentials, err.Error())
		}
	}
	projects, err := d.UserProjects(userID)
	if err != nil {
		return NewAuthenticationError(StatusNotAvailable, err.Error())
	} else if len(projects) == 0 {
		return NewAuthenticationError(StatusNoPermission, "User %s (%s@%s) does not have monitoring authorization on any project in any domain (required roles: %s)", userID, ba.Username, ba.DomainName, viper.GetString("keystone.roles"))
	}

	// default to first project (note that redundant attributes are not copied here to aovid errors)
	ba.Scope.ProjectID = projects[0].ProjectID
	if ba.Scope.ProjectID == "" {
		ba.Scope.DomainID = projects[0].DomainID
	}

	return nil
}

// authenticate authenticates a user using available authOptionsFromRequest (username+password or token)
// It returns the authorization context
func (d *keystone) authenticate(authOpts *tokens.AuthOptions, asServiceUser bool) (*policy.Context, string, AuthenticationError) {
	// check cache briefly
	if entry, found := d.tokenCache.Get(authOpts2StringKey(authOpts)); found {
		util.LogDebug("Token cache hit for %s", authOpts.TokenID)
		return entry.(*cacheEntry).context, entry.(*cacheEntry).endpointURL, nil
	}

	//use a custom token struct instead of tokens.Token which is way incomplete
	var tokenData keystoneToken
	var catalog *tokens.ServiceCatalog
	emptyScope := tokens.Scope{}
	if authOpts.TokenID != "" && authOpts.Scope == emptyScope && asServiceUser {
		util.LogDebug("verify token")
		// get token from token-ID which is being verified on that occasion
		if d.providerClient.TokenID == "" {
			err := d.reauthServiceUser().(AuthenticationError)
			if err != nil {
				return nil, "", err
			}
		}
		response := tokens.Get(d.providerClient, authOpts.TokenID)
		if response.Err != nil {
			//this includes 4xx responses, so after this point, we can be sure that the token is valid
			return nil, "", NewAuthenticationError(StatusWrongCredentials, response.Err.Error())
		}
		err := response.ExtractInto(&tokenData)
		if err != nil {
			return nil, "", NewAuthenticationError(StatusNotAvailable, err.Error())
		}
		catalog, err = response.ExtractServiceCatalog()
		if err != nil {
			return nil, "", NewAuthenticationError(StatusNotAvailable, err.Error())
		}
	} else {
		util.LogDebug("authenticate %s%s with scope %s.", authOpts.Username, authOpts.UserID, authOpts.Scope)
		client, err := newKeystoneClient()
		if err != nil {
			return nil, "", NewAuthenticationError(StatusNotAvailable, err.Error())
		}
		// create new token from basic authentication credentials or token ID
		response := tokens.Create(client, authOpts)
		// ugly copy & paste because the base-type of CreateResult and GetResult is private
		if response.Err != nil {
			statusCode := StatusWrongCredentials
			//this includes 4xx responses, so after this point, we can be sure that the token is valid
			if authOpts.Username != "" || authOpts.UserID != "" {
				util.LogInfo("Failed login of user %s@%s%s for scope %s: %s", authOpts.Username, authOpts.DomainName, authOpts.UserID, authOpts.Scope, response.Err.Error())
			} else if authOpts.TokenID != "" {
				util.LogInfo("Failed login of with token %s... for scope %s: %s", authOpts.TokenID[:1+len(authOpts.TokenID)/4], authOpts.Scope, response.Err.Error())
			} else {
				statusCode = StatusMissingCredentials
			}
			return nil, "", NewAuthenticationError(statusCode, response.Err.Error())
		}
		err = response.ExtractInto(&tokenData)
		if err != nil {
			return nil, "", NewAuthenticationError(StatusNotAvailable, err.Error())
		}
		catalog, err = response.ExtractServiceCatalog()
		if err != nil {
			return nil, "", NewAuthenticationError(StatusNotAvailable, err.Error())
		}
		// the token is passed separately
		tokenData.Token = response.Header.Get("X-Subject-Token")
	}

	// authorization context
	context := tokenData.ToContext()

	// service endpoint
	endpointURL, err := openstack.V3EndpointURL(catalog, gophercloud.EndpointOpts{Type: "metrics", Availability: gophercloud.AvailabilityPublic})
	if err != nil {
		return nil, "", NewAuthenticationError(StatusNotAvailable, err.Error())
	}
	// update the cache
	ce := cacheEntry{
		context:     &context,
		endpointURL: endpointURL,
	}
	d.tokenCache.Set(authOpts2StringKey(authOpts), &ce, cache.DefaultExpiration)
	return &context, endpointURL, nil
}

func (d *keystone) ChildProjects(projectID string) ([]string, error) {
	if ce, ok := d.projectTreeCache.Get(projectID); ok {
		return ce.([]string), nil
	}

	projects, err := d.fetchChildProjects(projectID)
	if err != nil {
		util.LogError("Unable to obtain project tree of project %s: %v", projectID, err)
		return nil, err
	}

	d.projectTreeCache.Set(projectID, projects, cache.DefaultExpiration)
	return projects, nil
}

func (d *keystone) fetchChildProjects(projectID string) ([]string, error) {
	projectIDs := []string{}
	enabledVal := true
	err := projects.List(d.providerClient, projects.ListOpts{ParentID: projectID, Enabled: &enabledVal}).EachPage(func(page pagination.Page) (bool, error) {
		slice, err := projects.ExtractProjects(page)
		if err != nil {
			return false, err
		}
		for _, p := range slice {
			projectIDs = append(projectIDs, p.ID)
			children, err := d.fetchChildProjects(p.ID)
			if err != nil {
				return false, err
			}
			projectIDs = append(projectIDs, children...)
		}

		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return projectIDs, nil
}

func (d *keystone) UserProjects(userID string) ([]tokens.Scope, error) {
	if up, ok := d.userProjectsCache.Get(userID); ok {
		return up.([]tokens.Scope), nil
	}

	up, err := d.fetchUserProjects(userID)
	if err != nil {
		util.LogError("Unable to obtain monitoring project list of user %s: %v", userID, err)
		return nil, err
	}

	// cache should be updated at this point
	d.userProjectsCache.Set(userID, up, cache.DefaultExpiration)
	return up, nil
}

func (d *keystone) fetchUserProjects(userID string) ([]tokens.Scope, error) {
	scopes := []tokens.Scope{}
	effectiveVal := true
	err := roles.ListAssignments(d.providerClient, roles.ListAssignmentsOpts{UserID: userID, Effective: &effectiveVal}).EachPage(func(page pagination.Page) (bool, error) {
		slice, err := roles.ExtractRoleAssignments(page)
		if err != nil {
			return false, err
		}
		for _, ra := range slice {
			if _, ok := d.monitoringRoles[ra.Role.ID]; ok && ra.Scope.Project.ID != "" {
				project, err := projects.Get(d.providerClient, ra.Scope.Project.ID).Extract()
				if err != nil {
					return false, err
				}
				domainName := d.domainNames[project.DomainID]
				scopes = append(scopes, tokens.Scope{ProjectID: ra.Scope.Project.ID, ProjectName: project.Name,
					DomainID: project.DomainID, DomainName: domainName})
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return scopes, nil
}

func (d *keystone) UserID(username, userDomain string) (string, error) {
	key := username + "@" + userDomain
	if ce, ok := d.userIDCache.Get(key); ok {
		return ce.(string), nil
	}

	id, err := d.fetchUserID(username, userDomain)
	if err != nil {
		return "", err
	}

	d.userIDCache.Set(key, id, cache.DefaultExpiration)

	return id, nil
}

func (d *keystone) fetchUserID(username string, userDomain string) (string, error) {
	userDomainID := d.domainIDs[userDomain]
	userID := ""
	enabled := true
	err := users.List(d.providerClient, users.ListOpts{Name: username, DomainID: userDomainID, Enabled: &enabled}).EachPage(func(page pagination.Page) (bool, error) {
		users, err := users.ExtractUsers(page)
		if err != nil {
			return false, err
		}
		for _, user := range users {
			userID = user.ID
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return "", err
	}

	if userID == "" {
		err = fmt.Errorf("no such user %s@%s", username, userDomain)
	}

	return userID, err
}
