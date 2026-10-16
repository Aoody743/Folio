package plugins

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const mongoBackupMagic = "PowerLinuxMongoBackup:BSON:v1\n"

func mongoBackupFrame(writer io.Writer, kind byte, document []byte) error {
	if len(document) > 32<<20 {
		return failure("backup_limit", "MongoDB frame exceeds 32 MiB", 413)
	}
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(document)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(document)
	return err
}
func mongoRestoreFrame(reader io.Reader) (byte, bson.Raw, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length < 5 || length > 32<<20 {
		return 0, nil, failure("artifact_invalid", "MongoDB backup contains an invalid frame length", 412)
	}
	raw := make([]byte, length)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return 0, nil, err
	}
	if bson.Raw(raw).Validate() != nil {
		return 0, nil, failure("artifact_invalid", "MongoDB backup contains invalid BSON", 412)
	}
	return header[0], raw, nil
}
func managedMongoProfile(instance Instance, record managedDatabaseRecord) DatabaseConnection {
	return DatabaseConnection{Engine: "mongodb", Host: managedDatabaseHost(instance), Port: setting(instance.Config, "port", 0), Database: record.Database, Username: record.Username, Access: "database"}
}
func (e *PrivilegedExecutor) backupMongoDatabase(ctx context.Context, instance Instance, record managedDatabaseRecord, writer io.Writer) error {
	client, err := openMongoProfile(ctx, managedMongoProfile(instance, record), record.Credential)
	if err != nil {
		return err
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	if err = verifyMongoLeastPrivilege(ctx, client, managedMongoProfile(instance, record)); err != nil {
		return err
	}
	database := client.Database(record.Database)
	collections, err := database.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return failure("backup_failed", "MongoDB collection metadata could not be read", 502)
	}
	if len(collections) > 10000 {
		return failure("backup_limit", "MongoDB database exceeds 10000 collections", 413)
	}
	if _, err = io.WriteString(writer, mongoBackupMagic); err != nil {
		return err
	}
	for _, collection := range collections {
		if strings.HasPrefix(collection.Name, "system.") {
			continue
		}
		metadata, err := bson.Marshal(bson.D{{Key: "name", Value: collection.Name}, {Key: "type", Value: collection.Type}, {Key: "options", Value: collection.Options}})
		if err != nil {
			return err
		}
		if err = mongoBackupFrame(writer, 'C', metadata); err != nil {
			return err
		}
		if collection.Type == "view" {
			continue
		}
		cursor, err := database.Collection(collection.Name).Find(ctx, bson.D{}, options.Find().SetBatchSize(128))
		if err != nil {
			return failure("backup_failed", "MongoDB document scan failed", 502)
		}
		for cursor.Next(ctx) {
			if err = mongoBackupFrame(writer, 'D', cursor.Current); err != nil {
				cursor.Close(ctx)
				return err
			}
		}
		err = cursor.Err()
		cursor.Close(ctx)
		if err != nil {
			return failure("backup_failed", "MongoDB backup scan was interrupted", 502)
		}
		indexes, err := database.Collection(collection.Name).Indexes().List(ctx)
		if err != nil {
			return failure("backup_failed", "MongoDB index metadata could not be read", 502)
		}
		for indexes.Next(ctx) {
			if err = mongoBackupFrame(writer, 'I', indexes.Current); err != nil {
				indexes.Close(ctx)
				return err
			}
		}
		err = indexes.Err()
		indexes.Close(ctx)
		if err != nil {
			return failure("backup_failed", "MongoDB index backup was interrupted", 502)
		}
	}
	return nil
}
func (e *PrivilegedExecutor) restoreMongoDatabase(ctx context.Context, instance Instance, record managedDatabaseRecord, reader io.ReadSeeker) error {
	return e.restoreMongoDatabaseMode(ctx, instance, record, reader, true)
}

func (e *PrivilegedExecutor) restoreMongoImportDatabase(ctx context.Context, instance Instance, record managedDatabaseRecord, reader io.ReadSeeker) error {
	return e.restoreMongoDatabaseMode(ctx, instance, record, reader, false)
}

func (e *PrivilegedExecutor) restoreMongoDatabaseMode(ctx context.Context, instance Instance, record managedDatabaseRecord, reader io.ReadSeeker, replaceExisting bool) error {
	validate := validateMongoImportArtifact
	if replaceExisting {
		validate = validateMongoBackupArtifact
	}
	if err := validate(ctx, reader); err != nil {
		return err
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return err
	}
	header := make([]byte, len(mongoBackupMagic))
	if _, err := io.ReadFull(reader, header); err != nil || string(header) != mongoBackupMagic {
		return failure("artifact_invalid", "Backup is not a native BSON database artifact", 412)
	}
	client, err := openMongoProfile(ctx, managedMongoProfile(instance, record), record.Credential)
	if err != nil {
		return err
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	if err = verifyMongoLeastPrivilege(ctx, client, managedMongoProfile(instance, record)); err != nil {
		return err
	}
	database := client.Database(record.Database)
	if replaceExisting {
		// Trusted Agent-owned backup restoration retains its explicit replace
		// behavior and database-only identity.
		names, listErr := database.ListCollectionNames(ctx, bson.D{})
		if listErr != nil {
			return failure("query_failed", "MongoDB restore preflight failed", 502)
		}
		for _, name := range names {
			if strings.HasPrefix(name, "system.") {
				continue
			}
			if err = database.Collection(name).Drop(ctx); err != nil {
				return failure("operation_recovery_required", "MongoDB restore could not clear a selected collection", 409)
			}
		}
	} else {
		names, listErr := database.ListCollectionNames(ctx, bson.D{})
		if listErr != nil {
			return failure("import_preflight_failed", "MongoDB target namespace could not be checked", 502)
		}
		if len(names) != 0 {
			return failure("import_target_not_empty", "External imports require an empty managed database; existing collections are preserved", 409)
		}
	}
	var current *mongo.Collection
	for {
		kind, raw, err := mongoRestoreFrame(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return failure("operation_recovery_required", "MongoDB backup ended unexpectedly during restore", 409)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch kind {
		case 'C':
			var metadata struct {
				Name    string `bson:"name"`
				Type    string `bson:"type"`
				Options bson.D `bson:"options"`
			}
			if bson.Unmarshal(raw, &metadata) != nil || metadata.Name == "" || strings.HasPrefix(metadata.Name, "system.") || strings.ContainsRune(metadata.Name, 0) {
				return failure("artifact_invalid", "MongoDB collection metadata is invalid", 412)
			}
			command := bson.D{{Key: "create", Value: metadata.Name}}
			command = append(command, metadata.Options...)
			if err = database.RunCommand(ctx, command).Err(); err != nil {
				return failure("operation_recovery_required", "MongoDB collection restoration failed", 409)
			}
			current = database.Collection(metadata.Name)
		case 'D':
			if current == nil {
				return failure("artifact_invalid", "MongoDB document has no collection", 412)
			}
			if _, err = current.InsertOne(ctx, raw); err != nil {
				return failure("operation_recovery_required", "MongoDB document restoration failed", 409)
			}
		case 'I':
			if current == nil {
				return failure("artifact_invalid", "MongoDB index has no collection", 412)
			}
			var index bson.D
			if bson.Unmarshal(raw, &index) != nil {
				return failure("artifact_invalid", "MongoDB index metadata is invalid", 412)
			}
			clean := bson.D{}
			skip := false
			for _, field := range index {
				if field.Key == "name" && field.Value == "_id_" {
					skip = true
				}
				if field.Key == "v" || field.Key == "ns" {
					continue
				}
				clean = append(clean, field)
			}
			if skip {
				continue
			}
			if err = database.RunCommand(ctx, bson.D{{Key: "createIndexes", Value: current.Name()}, {Key: "indexes", Value: bson.A{clean}}}).Err(); err != nil {
				return failure("operation_recovery_required", "MongoDB index restoration failed", 409)
			}
		default:
			return failure("artifact_invalid", "MongoDB backup contains an unknown frame", 412)
		}
	}
}

// validateMongoImportArtifact consumes the whole framed archive before restore
// connects or drops a collection. An uploaded file can therefore never make a
// late malformed frame turn into a partially destructively restored database.
func validateMongoImportArtifact(ctx context.Context, reader io.ReadSeeker) error {
	return validateMongoArchive(ctx, reader, true)
}

func validateMongoBackupArtifact(ctx context.Context, reader io.ReadSeeker) error {
	return validateMongoArchive(ctx, reader, false)
}

func validateMongoArchive(ctx context.Context, reader io.ReadSeeker, strict bool) error {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return err
	}
	header := make([]byte, len(mongoBackupMagic))
	if _, err := io.ReadFull(reader, header); err != nil || string(header) != mongoBackupMagic {
		return failure("artifact_invalid", "MongoDB import is not a native BSON archive", 412)
	}
	collections := map[string]struct{}{}
	current := ""
	frames := uint64(0)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind, raw, err := mongoRestoreFrame(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return failure("artifact_invalid", "MongoDB import contains a truncated or invalid frame", 412)
		}
		frames++
		if frames > 10_000_000 {
			return failure("artifact_invalid", "MongoDB import contains too many frames", 413)
		}
		if strict {
			if err = mongoBSONHasNoExecutableValues(raw, 0); err != nil {
				return failure("artifact_invalid", "MongoDB import contains executable BSON values", 412)
			}
		}
		switch kind {
		case 'C':
			var metadata struct {
				Name    string `bson:"name"`
				Type    string `bson:"type"`
				Options bson.D `bson:"options"`
			}
			if bson.Unmarshal(raw, &metadata) != nil || metadata.Name == "" || len(metadata.Name) > 255 || strings.HasPrefix(metadata.Name, "system.") || strings.ContainsRune(metadata.Name, 0) || metadata.Type != "collection" {
				return failure("artifact_invalid", "MongoDB import contains unsupported collection metadata or a view", 412)
			}
			if strict && validateMongoImportCollectionOptions(metadata.Options) != nil {
				return failure("artifact_invalid", "MongoDB import contains unsupported or unsafe collection options", 412)
			}
			if _, exists := collections[metadata.Name]; exists || len(collections) >= 10000 {
				return failure("artifact_invalid", "MongoDB import contains duplicate or excessive collections", 412)
			}
			collections[metadata.Name] = struct{}{}
			current = metadata.Name
		case 'D':
			if current == "" {
				return failure("artifact_invalid", "MongoDB import document has no collection", 412)
			}
		case 'I':
			if current == "" {
				return failure("artifact_invalid", "MongoDB import index has no collection", 412)
			}
			var index bson.D
			if bson.Unmarshal(raw, &index) != nil {
				return failure("artifact_invalid", "MongoDB import index metadata is invalid", 412)
			}
			if !strict {
				continue
			}
			hasName, hasKey := false, false
			for _, element := range index {
				switch element.Key {
				case "name":
					name, ok := element.Value.(string)
					hasName = ok && name != "" && len(name) <= 128 && !strings.ContainsRune(name, 0)
				case "key":
					key, ok := element.Value.(bson.D)
					hasKey = ok && len(key) > 0 && len(key) <= 128
					for _, part := range key {
						if part.Key == "" || len(part.Key) > 255 || strings.ContainsRune(part.Key, 0) {
							hasKey = false
						}
					}
				case "collation":
					if !mongoImportSafeCollation(element.Value) {
						return failure("artifact_invalid", "MongoDB import contains unsafe index collation options", 412)
					}
				case "v", "ns", "unique", "partialFilterExpression", "sparse", "expireAfterSeconds", "hidden", "weights", "default_language", "language_override", "textIndexVersion", "2dsphereIndexVersion", "bits", "min", "max", "bucketSize", "wildcardProjection":
				default:
					return failure("artifact_invalid", "MongoDB import contains unsupported index options", 412)
				}
			}
			if !hasName || !hasKey {
				return failure("artifact_invalid", "MongoDB import index is missing its name or key", 412)
			}
		default:
			return failure("artifact_invalid", "MongoDB import contains an unknown frame type", 412)
		}
	}
}

func validateMongoImportCollectionOptions(options bson.D) error {
	seen := map[string]bool{}
	for _, option := range options {
		if seen[option.Key] {
			return errors.New("duplicate MongoDB collection option")
		}
		seen[option.Key] = true
		switch option.Key {
		case "capped":
			if _, ok := option.Value.(bool); !ok {
				return errors.New("invalid capped option")
			}
		case "size", "max":
			value, ok := mongoImportInteger(option.Value)
			if !ok || value < 0 || value > maxExtractedBytes {
				return errors.New("invalid capped collection bound")
			}
		case "validationLevel":
			value, ok := option.Value.(string)
			if !ok || (value != "off" && value != "strict" && value != "moderate") {
				return errors.New("invalid validation level")
			}
		case "validationAction":
			value, ok := option.Value.(string)
			if !ok || (value != "error" && value != "warn") {
				return errors.New("invalid validation action")
			}
		case "validator":
			if !mongoImportSafeValidator(option.Value, 0) {
				return errors.New("validator uses an unsupported expression")
			}
		case "collation":
			if !mongoImportSafeCollation(option.Value) {
				return errors.New("invalid collation")
			}
		default:
			return errors.New("unsupported MongoDB collection option")
		}
	}
	return nil
}

func mongoImportInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	default:
		return 0, false
	}
}

func mongoImportSafeCollation(value any) bool {
	document, ok := value.(bson.D)
	if !ok || len(document) == 0 {
		return false
	}
	seen := map[string]bool{}
	hasLocale := false
	for _, element := range document {
		if seen[element.Key] {
			return false
		}
		seen[element.Key] = true
		switch element.Key {
		case "locale":
			locale, ok := element.Value.(string)
			if !ok || locale == "" || len(locale) > 64 || strings.ContainsRune(locale, 0) {
				return false
			}
			hasLocale = true
		case "caseLevel", "backwards", "numericOrdering", "normalization":
			if _, ok := element.Value.(bool); !ok {
				return false
			}
		case "caseFirst":
			text, ok := element.Value.(string)
			if !ok || (text != "off" && text != "upper" && text != "lower") {
				return false
			}
		case "strength":
			strength, ok := mongoImportInteger(element.Value)
			if !ok || strength < 1 || strength > 5 {
				return false
			}
		case "alternate":
			text, ok := element.Value.(string)
			if !ok || (text != "non-ignorable" && text != "shifted") {
				return false
			}
		case "maxVariable":
			text, ok := element.Value.(string)
			if !ok || (text != "punct" && text != "space") {
				return false
			}
		default:
			return false
		}
	}
	return hasLocale
}

func mongoImportSafeValidator(value any, depth int) bool {
	if depth > 50 {
		return false
	}
	switch typed := value.(type) {
	case bson.D:
		seen := map[string]bool{}
		for _, element := range typed {
			if element.Key == "" || strings.ContainsRune(element.Key, 0) || seen[element.Key] {
				return false
			}
			seen[element.Key] = true
			if strings.HasPrefix(element.Key, "$") {
				switch element.Key {
				case "$and", "$or", "$nor", "$not", "$eq", "$ne", "$gt", "$gte", "$lt", "$lte", "$in", "$nin", "$exists", "$type", "$mod", "$all", "$size", "$elemMatch":
				default:
					return false
				}
			}
			if !mongoImportSafeValidator(element.Value, depth+1) {
				return false
			}
		}
		return true
	case bson.A:
		for _, element := range typed {
			if !mongoImportSafeValidator(element, depth+1) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

func mongoBSONHasNoExecutableValues(raw bson.Raw, depth int) error {
	if depth > 100 {
		return errors.New("BSON nesting limit exceeded")
	}
	elements, err := raw.Elements()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, element := range elements {
		key := element.Key()
		if seen[key] {
			return errors.New("duplicate BSON key is not allowed")
		}
		seen[key] = true
		if key == "$where" || key == "$function" || key == "$accumulator" {
			return errors.New("$where is not allowed")
		}
		value, err := element.ValueErr()
		if err != nil {
			return err
		}
		switch value.Type {
		case bson.TypeJavaScript, bson.TypeCodeWithScope, bson.TypeDBPointer, bson.TypeSymbol, bson.TypeUndefined:
			return errors.New("executable or legacy BSON type is not allowed")
		case bson.TypeRegex:
			return errors.New("untrusted regular expressions are not allowed")
		case bson.TypeEmbeddedDocument:
			if err := mongoBSONHasNoExecutableValues(value.Document(), depth+1); err != nil {
				return err
			}
		case bson.TypeArray:
			values, err := value.Array().Values()
			if err != nil {
				return err
			}
			for _, nested := range values {
				if nested.Type == bson.TypeJavaScript || nested.Type == bson.TypeCodeWithScope || nested.Type == bson.TypeDBPointer || nested.Type == bson.TypeSymbol || nested.Type == bson.TypeUndefined || nested.Type == bson.TypeRegex {
					return errors.New("executable or legacy BSON type is not allowed")
				}
				if nested.Type == bson.TypeEmbeddedDocument {
					if err := mongoBSONHasNoExecutableValues(nested.Document(), depth+1); err != nil {
						return err
					}
				}
				if nested.Type == bson.TypeArray {
					if err := mongoBSONArrayHasNoExecutableValues(nested.Array(), depth+1); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func mongoBSONArrayHasNoExecutableValues(array bson.RawArray, depth int) error {
	if depth > 100 {
		return errors.New("BSON nesting limit exceeded")
	}
	values, err := array.Values()
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.Type == bson.TypeJavaScript || value.Type == bson.TypeCodeWithScope || value.Type == bson.TypeDBPointer || value.Type == bson.TypeSymbol || value.Type == bson.TypeUndefined || value.Type == bson.TypeRegex {
			return errors.New("executable or legacy BSON type is not allowed")
		}
		if value.Type == bson.TypeEmbeddedDocument {
			if err := mongoBSONHasNoExecutableValues(value.Document(), depth+1); err != nil {
				return err
			}
		}
		if value.Type == bson.TypeArray {
			if err := mongoBSONArrayHasNoExecutableValues(value.Array(), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
