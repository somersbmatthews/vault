package database

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/vault/sdk/database/dbplugin"

	"github.com/hashicorp/vault/sdk/database/newdbplugin"

	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/strutil"
	"github.com/hashicorp/vault/sdk/logical"
)

func pathCredsCreate(b *databaseBackend) []*framework.Path {
	return []*framework.Path{
		&framework.Path{
			Pattern: "creds/" + framework.GenericNameRegex("name"),
			Fields: map[string]*framework.FieldSchema{
				"name": &framework.FieldSchema{
					Type:        framework.TypeString,
					Description: "Name of the role.",
				},
			},

			Callbacks: map[logical.Operation]framework.OperationFunc{
				logical.ReadOperation: b.pathCredsCreateRead(),
			},

			HelpSynopsis:    pathCredsCreateReadHelpSyn,
			HelpDescription: pathCredsCreateReadHelpDesc,
		},
		&framework.Path{
			Pattern: "static-creds/" + framework.GenericNameRegex("name"),
			Fields: map[string]*framework.FieldSchema{
				"name": &framework.FieldSchema{
					Type:        framework.TypeString,
					Description: "Name of the static role.",
				},
			},

			Callbacks: map[logical.Operation]framework.OperationFunc{
				logical.ReadOperation: b.pathStaticCredsRead(),
			},

			HelpSynopsis:    pathStaticCredsReadHelpSyn,
			HelpDescription: pathStaticCredsReadHelpDesc,
		},
	}
}

func (b *databaseBackend) pathCredsCreateRead() framework.OperationFunc {
	return func(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
		name := data.Get("name").(string)

		// Get the role
		role, err := b.Role(ctx, req.Storage, name)
		if err != nil {
			return nil, err
		}
		if role == nil {
			return logical.ErrorResponse(fmt.Sprintf("unknown role: %s", name)), nil
		}

		dbConfig, err := b.DatabaseConfig(ctx, req.Storage, role.DBName)
		if err != nil {
			return nil, err
		}

		// If role name isn't in the database's allowed roles, send back a
		// permission denied.
		if !strutil.StrListContains(dbConfig.AllowedRoles, "*") && !strutil.StrListContainsGlob(dbConfig.AllowedRoles, name) {
			return nil, fmt.Errorf("%q is not an allowed role", name)
		}

		// Get the Database object
		db, err := b.GetConnection(ctx, req.Storage, role.DBName)
		if err != nil {
			return nil, err
		}

		db.RLock()
		defer db.RUnlock()

		ttl, _, err := framework.CalculateTTL(b.System(), 0, role.DefaultTTL, 0, role.MaxTTL, 0, time.Time{})
		if err != nil {
			return nil, err
		}
		expiration := time.Now().Add(ttl)
		// Adding a small buffer since the TTL will be calculated again after this call
		// to ensure the database credential does not expire before the lease
		expiration = expiration.Add(5 * time.Second)

		username, password, err := b.createUser(ctx, db.database, role.Statements, req.DisplayName, name, expiration, dbConfig.PasswordPolicy)
		if err != nil {
			b.CloseIfShutdown(db, err)
			return nil, err
		}

		resp := b.Secret(SecretCredsType).Response(map[string]interface{}{
			"username": username,
			"password": password,
		}, map[string]interface{}{
			"username":              username,
			"role":                  name,
			"db_name":               role.DBName,
			"revocation_statements": role.Statements.Revocation,
		})
		resp.Secret.TTL = role.DefaultTTL
		resp.Secret.MaxTTL = role.MaxTTL
		return resp, nil
	}
}

func (b databaseBackend) createUser(ctx context.Context, dbw databaseVersionWrapper, statements dbplugin.Statements, displayName, roleName string, expiration time.Time, passwordPolicy string) (username, password string, err error) {
	if dbw.database != nil {
		return b.createNewUser(ctx, dbw, statements, displayName, roleName, expiration, passwordPolicy)
	}
	return b.createLegacyUser(ctx, dbw, statements, displayName, roleName, expiration)
}

// createNewUser creates a user with the v5 Database interface
func (b databaseBackend) createNewUser(
	ctx context.Context,
	dbw databaseVersionWrapper,
	statements dbplugin.Statements,
	displayName, roleName string,
	expiration time.Time,
	passwordPolicy string) (username, password string, err error) {

	pass, err := generatePassword(ctx, b.System(), passwordPolicy)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate password: %w", err)
	}

	req := newdbplugin.NewUserRequest{
		UsernameConfig: newdbplugin.UsernameMetadata{
			DisplayName: displayName,
			RoleName:    roleName,
		},
		Statements: newdbplugin.Statements{
			Commands: statements.Creation,
		},
		RollbackStatements: newdbplugin.Statements{
			Commands: statements.Rollback,
		},
		Password:   pass,
		Expiration: expiration,
	}

	resp, err := dbw.database.NewUser(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("failed to create user: %w", err)
	}
	return resp.Username, pass, nil
}

// createLegacyUser creates a user with the v4 Database interface
func (b databaseBackend) createLegacyUser(
	ctx context.Context,
	dbw databaseVersionWrapper,
	statements dbplugin.Statements,
	displayName, roleName string,
	expiration time.Time) (username, password string, err error) {

	usernameConfig := dbplugin.UsernameConfig{
		DisplayName: displayName,
		RoleName:    roleName,
	}

	return dbw.legacyDatabase.CreateUser(ctx, statements, usernameConfig, expiration)
}

func (b *databaseBackend) pathStaticCredsRead() framework.OperationFunc {
	return func(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
		name := data.Get("name").(string)

		role, err := b.StaticRole(ctx, req.Storage, name)
		if err != nil {
			return nil, err
		}
		if role == nil {
			return logical.ErrorResponse("unknown role: %s", name), nil
		}

		dbConfig, err := b.DatabaseConfig(ctx, req.Storage, role.DBName)
		if err != nil {
			return nil, err
		}

		// If role name isn't in the database's allowed roles, send back a
		// permission denied.
		if !strutil.StrListContains(dbConfig.AllowedRoles, "*") && !strutil.StrListContainsGlob(dbConfig.AllowedRoles, name) {
			return nil, fmt.Errorf("%q is not an allowed role", name)
		}

		return &logical.Response{
			Data: map[string]interface{}{
				"username":            role.StaticAccount.Username,
				"password":            role.StaticAccount.Password,
				"ttl":                 role.StaticAccount.PasswordTTL().Seconds(),
				"rotation_period":     role.StaticAccount.RotationPeriod.Seconds(),
				"last_vault_rotation": role.StaticAccount.LastVaultRotation,
			},
		}, nil
	}
}

const pathCredsCreateReadHelpSyn = `
Request database credentials for a certain role.
`

const pathCredsCreateReadHelpDesc = `
This path reads database credentials for a certain role. The
database credentials will be generated on demand and will be automatically
revoked when the lease is up.
`

const pathStaticCredsReadHelpSyn = `
Request database credentials for a certain static role. These credentials are
rotated periodically.
`

const pathStaticCredsReadHelpDesc = `
This path reads database credentials for a certain static role. The database
credentials are rotated periodically according to their configuration, and will
return the same password until they are rotated.
`
