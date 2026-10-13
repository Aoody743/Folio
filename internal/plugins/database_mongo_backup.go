package plugins

import (
	"context"
	"encoding/binary"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"io"
	"strings"
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
func (e *PrivilegedExecutor) restoreMongoDatabase(ctx context.Context, instance Instance, record managedDatabaseRecord, reader io.Reader) error {
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
	// Restoration uses the original database-only role, never root. Existing
	// users and instance settings remain outside this operation's authority.
	names, err := database.ListCollectionNames(ctx, bson.D{})
	if err != nil {
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
