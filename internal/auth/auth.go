// Copyright 2014 Canonical Ltd.

package auth

import (
	"sort"
	"strings"

	"github.com/juju/loggo"
	"golang.org/x/net/context"
	"gopkg.in/errgo.v1"
	"gopkg.in/juju/idmclient.v1/params"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	"gopkg.in/macaroon-bakery.v2/bakery/identchecker"
	macaroon "gopkg.in/macaroon.v2"

	"github.com/CanonicalLtd/blues-identity/idp"
	"github.com/CanonicalLtd/blues-identity/store"
)

var logger = loggo.GetLogger("identity.internal.auth")

const (
	AdminUsername     = "admin@idm"
	SSHKeyGetterGroup = "sshkeygetter@idm"
	GroupListGroup    = "grouplist@idm"
)

var AdminProviderID = store.MakeProviderIdentity("idm", "admin")

const (
	kindGlobal = "global"
	kindUser   = "u"
)

// The following constants define possible operation actions.
const (
	ActionRead               = "read"
	ActionVerify             = "verify"
	ActionDischargeFor       = "dischargeFor"
	ActionDischarge          = "discharge"
	ActionCreateAgent        = "createAgent"
	ActionReadAdmin          = "readAdmin"
	ActionWriteAdmin         = "writeAdmin"
	ActionReadGroups         = "readGroups"
	ActionWriteGroups        = "writeGroups"
	ActionReadSSHKeys        = "readSSHKeys"
	ActionWriteSSHKeys       = "writeSSHKeys"
	ActionLogin              = "login"
	ActionReadDischargeToken = "read-discharge-token"
)

// TODO(mhilton) make the admin ACL configurable
var AdminACL = []string{AdminUsername}

// An Authorizer is used to authorize operations in the identity server.
type Authorizer struct {
	adminUsername  string
	adminPassword  string
	location       string
	checker        *identchecker.Checker
	store          store.Store
	groupResolvers map[string]groupResolver
}

// Params specifify the configuration parameters for a new Authroizer.
type Params struct {
	// AdminUsername is the username of the admin user in the
	// identity server.
	AdminUsername string

	// AdminPassword is the password of the admin user in the
	// identity server.
	AdminPassword string

	// Location is the url of the discharger that third-party caveats
	// will be addressed to. This should be the address of this
	// identity server.
	Location string

	// MacaroonOpStore is the store of macaroon operations and root
	// keys.
	MacaroonVerifier bakery.MacaroonVerifier

	// Store is the identity store.
	Store store.Store

	// IdentityProviders contains the set of identity providers that
	// are configured for the service. The authenticatore uses these
	// to get group information for authenticated users.
	IdentityProviders []idp.IdentityProvider
}

// New creates a new Authorizer for authorizing identity server
// operations.
func New(params Params) *Authorizer {
	a := &Authorizer{
		adminUsername: params.AdminUsername,
		adminPassword: params.AdminPassword,
		location:      params.Location,
		store:         params.Store,
	}
	resolvers := make(map[string]groupResolver)
	for _, idp := range params.IdentityProviders {
		idp := idp
		resolvers[idp.Name()] = idpGroupResolver{idp}
	}
	// Add a group resolver for the built-in idm provider.
	resolvers["idm"] = idmGroupResolver{
		store:     params.Store,
		resolvers: resolvers,
	}

	a.groupResolvers = resolvers
	a.checker = identchecker.NewChecker(identchecker.CheckerParams{
		Checker: NewChecker(a),
		Authorizer: identchecker.ACLAuthorizer{
			GetACL: func(ctx context.Context, op bakery.Op) ([]string, bool, error) {
				return a.aclForOp(ctx, op)
			},
		},
		IdentityClient:   identityClient{a},
		MacaroonVerifier: params.MacaroonVerifier,
	})
	return a
}

func (a *Authorizer) aclForOp(ctx context.Context, op bakery.Op) (acl []string, public bool, _ error) {
	kind, name := splitEntity(op.Entity)
	switch kind {
	case kindGlobal:
		if name != "" {
			return nil, false, nil
		}
		switch op.Action {
		case ActionRead:
			// Only admins are allowed to read global information.
			return AdminACL, true, nil
		case ActionDischargeFor:
			// Only admins are allowed to discharge for other users.
			return AdminACL, true, nil
		case ActionVerify:
			// Everyone is allowed to verify a macaroon.
			return []string{identchecker.Everyone}, true, nil
		case ActionLogin:
			// Everyone is allowed to log in.
			return []string{identchecker.Everyone}, true, nil
		case ActionDischarge:
			// Everyone is allowed to discharge, but they must authenticate themselves
			// first.
			return []string{identchecker.Everyone}, false, nil
		case ActionCreateAgent:
			// Anyone can create an agent, as long as they've authenticated
			// themselves.
			return []string{identchecker.Everyone}, false, nil
		}
	case kindUser:
		if name == "" {
			return nil, false, nil
		}
		username := name
		acl := make([]string, 0, len(AdminACL)+2)
		acl = append(acl, AdminACL...)
		switch op.Action {
		case ActionRead:
			return append(acl, username), false, nil
		case ActionReadAdmin:
			return acl, false, nil
		case ActionWriteAdmin:
			return acl, false, nil
		case ActionReadGroups:
			// Administrators, users with GroupList permissions and the user
			// themselves can list their groups.
			return append(acl, username, GroupListGroup), false, nil
		case ActionWriteGroups:
			// Only administrators can set a user's groups.
			return acl, false, nil
		case ActionReadSSHKeys:
			return append(acl, username, SSHKeyGetterGroup), false, nil
		case ActionWriteSSHKeys:
			return append(acl, username), false, nil
		}
	case "groups":
		switch op.Action {
		case ActionDischarge:
			return strings.Fields(name), true, nil
		}
	}
	logger.Infof("no ACL found for op %#v", op)
	return nil, false, nil
}

// SetAdminPublicKey configures the public key on the admin user. This is
// to allow agent login as the admin user.
func (a *Authorizer) SetAdminPublicKey(ctx context.Context, pk *bakery.PublicKey) error {
	var pks []bakery.PublicKey
	if pk != nil {
		pks = append(pks, *pk)
	}
	return errgo.Mask(a.store.UpdateIdentity(
		ctx,
		&store.Identity{
			ProviderID: AdminProviderID,
			Username:   AdminUsername,
			PublicKeys: pks,
		},
		store.Update{
			store.Username:   store.Set,
			store.Groups:     store.Set,
			store.PublicKeys: store.Set,
		},
	))
}

// Auth checks that client, as identified by the given context and
// macaroons, is authorized to perform the given operations. It may
// return an bakery.DischargeRequiredError when further checks are
// required, or params.ErrUnauthorized if the user is authenticated but
// does not have the required authorization.
func (a *Authorizer) Auth(ctx context.Context, mss []macaroon.Slice, ops ...bakery.Op) (*identchecker.AuthInfo, error) {
	authInfo, err := a.checker.Auth(mss...).Allow(ctx, ops...)
	if err != nil {
		if errgo.Cause(err) == bakery.ErrPermissionDenied {
			return nil, errgo.WithCausef(err, params.ErrUnauthorized, "")
		}
		return nil, errgo.Mask(err, isDischargeRequiredError)
	}
	return authInfo, nil
}

func isDischargeRequiredError(err error) bool {
	_, ok := errgo.Cause(err).(*bakery.DischargeRequiredError)
	return ok
}

// Identity creates a new identity for the user with the given username,
// such a user must exist in the store.
func (a *Authorizer) Identity(ctx context.Context, username string) (*Identity, error) {
	id := &Identity{
		id: store.Identity{
			Username: username,
		},
		authorizer: a,
	}
	if err := id.lookup(ctx); err != nil {
		return nil, errgo.Mask(err, errgo.Is(params.ErrNotFound))
	}
	return id, nil
}

// An identityClient is an implementation of identchecker.IdentityClient that
// uses the identity server's data store to get identity information.
type identityClient struct {
	authorizer *Authorizer
}

// IdentityFromContext implements
// identchecker.IdentityClient.IdentityFromContext by looking for admin
// credentials in the context.
func (c identityClient) IdentityFromContext(ctx context.Context) (_ident identchecker.Identity, _ []checkers.Caveat, _ error) {
	if username := usernameFromContext(ctx); username != "" {
		if err := CheckUserDomain(ctx, username); err != nil {
			return nil, nil, errgo.Mask(err)
		}
		id, err := c.authorizer.Identity(ctx, username)
		if err != nil {
			return nil, nil, errgo.Mask(err, errgo.Is(params.ErrNotFound))
		}
		return id, nil, nil
	}
	if username, password, ok := userCredentialsFromContext(ctx); ok {
		if username == c.authorizer.adminUsername && password == c.authorizer.adminPassword {
			return &Identity{
				id: store.Identity{
					Username: AdminUsername,
				},
				authorizer: c.authorizer,
			}, nil, nil
		}
		return nil, nil, errgo.WithCausef(nil, params.ErrUnauthorized, "invalid credentials")
	}
	return nil, []checkers.Caveat{
		checkers.NeedDeclaredCaveat(
			checkers.Caveat{
				Location:  c.authorizer.location,
				Condition: "is-authenticated-user",
			},
			"username",
		),
	}, nil
}

// CheckUserDomain checks that the given user name has
// a valid domain name with respect to the given context
// (see also ContextWithRequiredDomain).
func CheckUserDomain(ctx context.Context, username string) error {
	domain, ok := ctx.Value(requiredDomainKey).(string)
	if ok && !strings.HasSuffix(username, "@"+domain) {
		return errgo.Newf("%q not in required domain %q", username, domain)
	}
	return nil
}

// DeclaredIdentity implements identchecker.IdentityClient.DeclaredIdentity by
// retrieving the user information from the declared map.
func (c identityClient) DeclaredIdentity(ctx context.Context, declared map[string]string) (identchecker.Identity, error) {
	username, ok := declared["username"]
	if !ok {
		return nil, errgo.Newf("no declared user")
	}
	if err := CheckUserDomain(ctx, username); err != nil {
		return nil, errgo.Mask(err)
	}
	return &Identity{
		id: store.Identity{
			Username: username,
		},
		authorizer: c.authorizer,
	}, nil
}

// An Identity is the implementation of identchecker.Identity used in the
// identity server.
type Identity struct {
	// Initially id is populated only with the Username field,
	// but calls that require more information call Identity.lookup
	// which fills out the rest.
	id             store.Identity
	authorizer     *Authorizer
	resolvedGroups []string
}

// Id implements identchecker.Identity.Id.
func (id *Identity) Id() string {
	return string(id.id.Username)
}

// Domain implements identchecker.Identity.Domain.
func (id *Identity) Domain() string {
	return ""
}

// Allow implements identchecker.ACLIdentity.Allow by checking whether the
// given identity is in any of the required groups or users.
func (id *Identity) Allow(ctx context.Context, acl []string) (bool, error) {
	if ok, isTrivial := trivialAllow(id.id.Username, acl); isTrivial {
		return ok, nil
	}
	groups, err := id.Groups(ctx)
	if err != nil {
		return false, errgo.Mask(err)
	}
	for _, a := range acl {
		for _, g := range groups {
			if g == a {
				return true, nil
			}
		}
	}
	return false, nil
}

// Groups returns all the groups associated with the user. The groups
// include those stored in the identity server's database along with any
// retrieved by the relevent identity provider's GetGroups method. Once
// the set of groups has been determined it is cached in the Identity.
func (id *Identity) Groups(ctx context.Context) ([]string, error) {
	if id.resolvedGroups != nil {
		return id.resolvedGroups, nil
	}
	if err := id.lookup(ctx); err != nil {
		return nil, errgo.Mask(err, errgo.Is(params.ErrNotFound))
	}
	groups := id.id.Groups
	if gr := id.authorizer.groupResolvers[id.id.ProviderID.Provider()]; gr != nil {
		var err error
		groups, err = gr.resolveGroups(ctx, &id.id)
		if err != nil {
			logger.Warningf("error resolving groups: %s", err)
		} else {
			id.resolvedGroups = groups
		}
	}
	return groups, nil
}

// StoreIdentity returns the store identity document.
// Callers must not mutate the contents of the returned
// value.
func (id *Identity) StoreIdentity(ctx context.Context) (*store.Identity, error) {
	if err := id.lookup(ctx); err != nil {
		return nil, errgo.Mask(err)
	}
	return &id.id, nil
}

func (id *Identity) lookup(ctx context.Context) error {
	if id.id.ID != "" {
		return nil
	}
	if err := id.authorizer.store.Identity(ctx, &id.id); err != nil {
		if errgo.Cause(err) == store.ErrNotFound {
			return errgo.WithCausef(err, params.ErrNotFound, "")
		}
		return errgo.Mask(err)
	}
	return nil
}

// trivialAllow reports whether the username should be allowed
// access to the given ACL based on a superficial inspection
// of the ACL. If there is a definite answer, it will return
// a true isTrivial; otherwise it will return (false, false).
func trivialAllow(username string, acl []string) (allow, isTrivial bool) {
	if len(acl) == 0 {
		return false, true
	}
	for _, name := range acl {
		if name == "everyone" || name == username {
			return true, true
		}
	}
	return false, false
}

// DomainDischargeOp creates an operation that is discharging the
// specified domain.
func DomainDischargeOp(domain string) bakery.Op {
	return op("domain-"+domain, ActionDischarge)
}

// GroupsDischargeOp creates an operation that is discharging as a user
// in one of the specified groups.
func GroupsDischargeOp(groups []string) bakery.Op {
	return op("groups-"+strings.Join(groups, " "), ActionDischarge)
}

func UserOp(u params.Username, action string) bakery.Op {
	return op(kindUser+"-"+string(u), action)
}

func GlobalOp(action string) bakery.Op {
	return op(kindGlobal, action)
}

func op(entity, action string) bakery.Op {
	return bakery.Op{
		Entity: entity,
		Action: action,
	}
}

func splitEntity(entity string) (string, string) {
	if i := strings.Index(entity, "-"); i > 0 {
		return entity[0:i], entity[i+1:]
	}
	return entity, ""
}

// A groupResolver is used to update the groups associated with an
// identity.
type groupResolver interface {
	// resolveGroups returns the group information for the given
	// identity. If a non-nil error is returned it will be logged,
	// but the returned list of groups will still be taken as the set
	// of groups to be associated with the identity.
	resolveGroups(context.Context, *store.Identity) ([]string, error)
}

type idmGroupResolver struct {
	store     store.Store
	resolvers map[string]groupResolver
}

// resolveGroups implements groupResolver by checking returning
// groups that are in both the identity and the owner of the
// identity.
func (r idmGroupResolver) resolveGroups(ctx context.Context, identity *store.Identity) ([]string, error) {
	if len(identity.ProviderInfo["owner"]) == 0 {
		// No owner - no groups. This applies to admin@idm, but for
		// other users, it's probably an internal inconsistency error.
		return nil, nil
	}
	ownerID := store.ProviderIdentity(identity.ProviderInfo["owner"][0])
	if ownerID == AdminProviderID {
		// The admin user is a member of all groups by definition.
		return identity.Groups, nil
	}
	ownerIdentity := store.Identity{
		ProviderID: ownerID,
	}
	if err := r.store.Identity(ctx, &ownerIdentity); err != nil {
		if errgo.Cause(err) != store.ErrNotFound {
			return nil, errgo.Mask(err)
		}
		return nil, nil
	}
	resolver := r.resolvers[ownerID.Provider()]
	if resolver == nil {
		// Owner is somehow in an unknown provider.
		// TODO log/return an error?
		return nil, nil
	}
	ownerGroups, err := resolver.resolveGroups(ctx, &ownerIdentity)
	if err != nil {
		return nil, errgo.Mask(err)
	}
	allowedGroups := make([]string, 0, len(identity.Groups))
	for _, g1 := range identity.Groups {
		for _, g2 := range ownerGroups {
			if g2 == g1 {
				allowedGroups = append(allowedGroups, g1)
				break
			}
		}
	}
	return allowedGroups, nil
}

type idpGroupResolver struct {
	idp idp.IdentityProvider
}

// resolveGroups implements groupResolver by getting the groups from the
// idp and adding them to the set stored in the identity server.
func (r idpGroupResolver) resolveGroups(ctx context.Context, id *store.Identity) ([]string, error) {
	groups, err := r.idp.GetGroups(ctx, id)
	if err != nil {
		// We couldn't get the groups, so return only those stored in the database.
		return id.Groups, errgo.Mask(err)
	}
	return uniqueStrings(append(groups, id.Groups...)), nil
}

// uniqueStrings removes all duplicates from the supplied
// string slice, updating the slice in place.
// The values will be in lexicographic order.
func uniqueStrings(ss []string) []string {
	if len(ss) < 2 {
		return ss
	}
	sort.Strings(ss)
	prev := ss[0]
	out := ss[:1]
	for _, s := range ss[1:] {
		if s == prev {
			continue
		}
		out = append(out, s)
		prev = s
	}
	return out
}
