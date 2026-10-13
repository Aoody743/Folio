package plugins

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var mongoCollectionName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,119}$`)

type mongoInput struct {
	ID         string            `json:"id"`
	Collection string            `json:"collection"`
	Filter     json.RawMessage   `json:"filter,omitempty"`
	Projection json.RawMessage   `json:"projection,omitempty"`
	Sort       json.RawMessage   `json:"sort,omitempty"`
	Documents  []json.RawMessage `json:"documents,omitempty"`
	Update     json.RawMessage   `json:"update,omitempty"`
	Limit      int64             `json:"limit,omitempty"`
	Offset     int64             `json:"offset,omitempty"`
	Upsert     bool              `json:"upsert,omitempty"`
}

func mongoBaseSchema() map[string]any {
	return map[string]any{"id": idSchema(), "collection": map[string]any{"type": "string", "pattern": mongoCollectionName.String()}, "filter": map[string]any{"type": "object", "maxProperties": 64}}
}
func mongoFindSchema() map[string]any {
	p := mongoBaseSchema()
	p["projection"] = map[string]any{"type": "object", "maxProperties": 64}
	p["sort"] = map[string]any{"type": "object", "maxProperties": 16}
	p["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}
	p["offset"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 100000}
	return obj(p, "id", "collection")
}
func mongoInsertSchema() map[string]any {
	p := mongoBaseSchema()
	delete(p, "filter")
	p["documents"] = map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "items": map[string]any{"type": "object", "maxProperties": 128}}
	return obj(p, "id", "collection", "documents")
}
func mongoUpdateSchema() map[string]any {
	p := mongoBaseSchema()
	p["update"] = map[string]any{"type": "object", "minProperties": 1, "maxProperties": 32}
	p["upsert"] = map[string]any{"type": "boolean"}
	return obj(p, "id", "collection", "filter", "update")
}
func mongoDeleteSchema() map[string]any { return obj(mongoBaseSchema(), "id", "collection", "filter") }
func openMongoProfile(ctx context.Context, p DatabaseConnection, password string) (*mongo.Client, error) {
	u := url.URL{Scheme: "mongodb", User: url.UserPassword(p.Username, password), Host: net.JoinHostPort(p.Host, strconv.Itoa(p.Port)), Path: "/" + p.Database}
	q := u.Query()
	q.Set("authSource", p.Database)
	q.Set("directConnection", "true")
	u.RawQuery = q.Encode()
	o := options.Client().ApplyURI(u.String()).SetConnectTimeout(5 * time.Second).SetServerSelectionTimeout(5 * time.Second)
	if p.TLS {
		o.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: p.Host})
	}
	client, e := mongo.Connect(o)
	if e != nil {
		return nil, failure("connection_failed", "MongoDB connection could not be initialized", 502)
	}
	if e = client.Ping(ctx, nil); e != nil {
		client.Disconnect(context.WithoutCancel(ctx))
		return nil, failure("connection_failed", "MongoDB TLS, authentication or connection failed", 502)
	}
	return client, nil
}
func verifyMongoLeastPrivilege(ctx context.Context, client *mongo.Client, p DatabaseConnection) error {
	var result struct {
		AuthInfo struct {
			Roles []struct {
				Role string `bson:"role"`
				DB   string `bson:"db"`
			} `bson:"authenticatedUserRoles"`
			Privileges []struct {
				Resource struct {
					AnyResource bool   `bson:"anyResource"`
					DB          string `bson:"db"`
					Collection  string `bson:"collection"`
				} `bson:"resource"`
			} `bson:"authenticatedUserPrivileges"`
		} `bson:"authInfo"`
	}
	if e := client.Database(p.Database).RunCommand(ctx, bson.D{{Key: "connectionStatus", Value: 1}, {Key: "showPrivileges", Value: true}}).Decode(&result); e != nil {
		return failure("credential_verification_failed", "MongoDB identity privileges could not be verified", 502)
	}
	if len(result.AuthInfo.Roles) == 0 {
		return failure("credential_scope", "MongoDB database actions require a verified database-only role", 403)
	}
	for _, role := range result.AuthInfo.Roles {
		if role.DB != p.Database || (role.Role != "read" && role.Role != "readWrite") {
			return failure("credential_scope", "MongoDB database actions require only read/readWrite roles on the selected database", 403)
		}
	}
	for _, privilege := range result.AuthInfo.Privileges {
		if privilege.Resource.AnyResource || privilege.Resource.DB != p.Database {
			return failure("credential_scope", "MongoDB identity has privileges outside the selected database", 403)
		}
	}
	return nil
}
func probeMongoProfile(ctx context.Context, p DatabaseConnection, password string) error {
	client, e := openMongoProfile(ctx, p, password)
	if e != nil {
		return e
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	if p.Access == "database" {
		return verifyMongoLeastPrivilege(ctx, client, p)
	}
	return nil
}
func (m *Manager) mongoConnection(ctx context.Context, a core.Actor, p DatabaseConnection, scoped bool) (*mongo.Client, error) {
	if p.Engine != "mongodb" || scoped && p.Access != "database" {
		return nil, failure("credential_scope", "MongoDB document operations require database-only credentials", 403)
	}
	secret, e := m.deps.Secrets.Get(ctx, a, databaseProfileResource(p), p.CredentialRef)
	if e != nil {
		return nil, e
	}
	client, e := openMongoProfile(ctx, p, string(secret))
	for i := range secret {
		secret[i] = 0
	}
	if e != nil {
		return nil, e
	}
	if scoped {
		if e = verifyMongoLeastPrivilege(ctx, client, p); e != nil {
			client.Disconnect(context.WithoutCancel(ctx))
			return nil, e
		}
	}
	return client, nil
}
func (m *Manager) mongoStatus(ctx context.Context, a core.Actor, p DatabaseConnection) (core.ActionResult, error) {
	client, e := m.mongoConnection(ctx, a, p, p.Access == "database")
	if e != nil {
		return core.ActionResult{}, e
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	return core.ActionResult{Data: map[string]any{"id": p.ID, "engine": p.Engine, "connected": true, "database": p.Database, "user": p.Username, "sampled_at": time.Now().UTC()}, Resources: []string{databaseProfileResource(p)}}, nil
}
func (m *Manager) mongoCollections(ctx context.Context, a core.Actor, p DatabaseConnection) (core.ActionResult, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, e := m.mongoConnection(bounded, a, p, true)
	if e != nil {
		return core.ActionResult{}, e
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	names, e := client.Database(p.Database).ListCollectionNames(bounded, bson.D{})
	if e != nil {
		return core.ActionResult{}, failure("query_failed", "MongoDB collection listing failed", 502)
	}
	if len(names) > 1000 {
		return core.ActionResult{}, failure("result_limit", "Collection count exceeds 1000; refine the query", 413)
	}
	return core.ActionResult{Data: map[string]any{"items": names, "total": len(names)}, Resources: []string{databaseProfileResource(p)}}, nil
}
func mongoDocument(raw json.RawMessage) (bson.D, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	var doc bson.D
	if bson.UnmarshalExtJSON(raw, false, &doc) != nil {
		return nil, failure("invalid_input", "MongoDB input must be a valid Extended JSON object", 400)
	}
	return doc, nil
}
func (m *Manager) mongoInputs(ctx context.Context, a core.Actor, raw json.RawMessage) (mongoInput, DatabaseConnection, *mongo.Client, error) {
	var in mongoInput
	if e := parse(raw, &in); e != nil {
		return in, DatabaseConnection{}, nil, e
	}
	if !mongoCollectionName.MatchString(in.Collection) {
		return in, DatabaseConnection{}, nil, failure("invalid_input", "Invalid collection name", 400)
	}
	p, e := m.databaseProfile(ctx, in.ID)
	if e != nil {
		return in, p, nil, e
	}
	client, e := m.mongoConnection(ctx, a, p, true)
	return in, p, client, e
}
func (m *Manager) mongoFind(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	in, p, client, e := m.mongoInputs(bounded, a, raw)
	if e != nil {
		return core.ActionResult{}, e
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	filter, e := mongoDocument(in.Filter)
	if e != nil {
		return core.ActionResult{}, e
	}
	limit := in.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 || in.Offset < 0 || in.Offset > 100000 {
		return core.ActionResult{}, failure("invalid_input", "MongoDB pagination exceeds limits", 400)
	}
	o := options.Find().SetLimit(limit + 1).SetSkip(in.Offset)
	if len(in.Projection) > 0 {
		projection, e := mongoDocument(in.Projection)
		if e != nil {
			return core.ActionResult{}, e
		}
		o.SetProjection(projection)
	}
	if len(in.Sort) > 0 {
		sort, e := mongoDocument(in.Sort)
		if e != nil {
			return core.ActionResult{}, e
		}
		o.SetSort(sort)
	}
	cursor, e := client.Database(p.Database).Collection(in.Collection).Find(bounded, filter, o)
	if e != nil {
		return core.ActionResult{}, failure("query_failed", "MongoDB query failed", 400)
	}
	defer cursor.Close(context.WithoutCancel(ctx))
	items := []json.RawMessage{}
	total := 0
	truncated := false
	for cursor.Next(bounded) {
		if int64(len(items)) >= limit {
			truncated = true
			break
		}
		encoded, e := bson.MarshalExtJSON(cursor.Current, true, false)
		if e != nil {
			return core.ActionResult{}, failure("query_failed", "MongoDB document could not be decoded", 502)
		}
		total += len(encoded)
		if total > 1<<20 {
			return core.ActionResult{}, failure("result_limit", "MongoDB result exceeds 1 MiB", 413)
		}
		items = append(items, encoded)
	}
	if cursor.Err() != nil {
		return core.ActionResult{}, failure("query_failed", "MongoDB query failed or timed out", 502)
	}
	return core.ActionResult{Data: map[string]any{"items": items, "limit": limit, "offset": in.Offset, "truncated": truncated}, Resources: []string{databaseProfileResource(p)}}, nil
}
func (m *Manager) mongoInsert(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	in, p, client, e := m.mongoInputs(bounded, a, raw)
	if e != nil {
		return core.ActionResult{}, e
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	if len(in.Documents) < 1 || len(in.Documents) > 100 {
		return core.ActionResult{}, failure("invalid_input", "Insert requires 1–100 documents", 400)
	}
	documents := make([]any, len(in.Documents))
	for i, raw := range in.Documents {
		doc, e := mongoDocument(raw)
		if e != nil {
			return core.ActionResult{}, e
		}
		documents[i] = doc
	}
	result, e := client.Database(p.Database).Collection(in.Collection).InsertMany(bounded, documents)
	if e != nil {
		return core.ActionResult{}, failure("query_failed", "MongoDB insert failed", 400)
	}
	return core.ActionResult{Data: map[string]any{"inserted_count": len(result.InsertedIDs)}, Resources: []string{databaseProfileResource(p)}, AuditSummary: map[string]any{"collection": in.Collection, "inserted_count": len(result.InsertedIDs)}}, nil
}
func (m *Manager) mongoUpdate(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	in, p, client, e := m.mongoInputs(bounded, a, raw)
	if e != nil {
		return core.ActionResult{}, e
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	filter, e := mongoDocument(in.Filter)
	if e != nil {
		return core.ActionResult{}, e
	}
	update, e := mongoDocument(in.Update)
	if e != nil {
		return core.ActionResult{}, e
	}
	result, e := client.Database(p.Database).Collection(in.Collection).UpdateMany(bounded, filter, update, options.UpdateMany().SetUpsert(in.Upsert))
	if e != nil {
		return core.ActionResult{}, failure("query_failed", "MongoDB update failed", 400)
	}
	return core.ActionResult{Data: map[string]any{"matched_count": result.MatchedCount, "modified_count": result.ModifiedCount, "upserted_count": result.UpsertedCount}, Resources: []string{databaseProfileResource(p)}, AuditSummary: map[string]any{"collection": in.Collection, "modified_count": result.ModifiedCount}}, nil
}
func (m *Manager) mongoDelete(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	in, p, client, e := m.mongoInputs(bounded, a, raw)
	if e != nil {
		return core.ActionResult{}, e
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	filter, e := mongoDocument(in.Filter)
	if e != nil {
		return core.ActionResult{}, e
	}
	result, e := client.Database(p.Database).Collection(in.Collection).DeleteMany(bounded, filter)
	if e != nil {
		return core.ActionResult{}, failure("query_failed", "MongoDB delete failed", 400)
	}
	return core.ActionResult{Data: map[string]any{"deleted_count": result.DeletedCount}, Resources: []string{databaseProfileResource(p)}, AuditSummary: map[string]any{"collection": in.Collection, "deleted_count": result.DeletedCount}}, nil
}
