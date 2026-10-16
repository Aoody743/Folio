package plugins

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSanitizeMySQLImportKeepsBoundedTableAndLiteralRows(t *testing.T) {
	input := "-- dump header\n/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE */;\nDROP TABLE IF EXISTS `items`;\n" +
		"CREATE TABLE `items` (`id` int(11) NOT NULL, `label` varchar(255), PRIMARY KEY (`id`)) ENGINE=InnoDB;\n" +
		"INSERT INTO `items` (`id`,`label`) VALUES (1,'semi;colon -- literal'),(2,'/*!not a comment*/');\n"
	var output bytes.Buffer
	if err := sanitizeMySQLImport(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatalf("safe dump subset was rejected: code=%s err=%v", core.ErrorCode(err), err)
	}
	text := output.String()
	for _, want := range []string{"CREATE TABLE `items`", "INSERT INTO `items`", "semi;colon -- literal", "/*!not a comment*/"} {
		if !strings.Contains(text, want) {
			t.Fatalf("sanitized import lost safe SQL %q: %s", want, text)
		}
	}
	if strings.Contains(strings.ToUpper(text), "OLD_SQL_MODE") {
		t.Fatal("executable version comment was preserved")
	}
	if strings.Contains(strings.ToUpper(text), "DROP TABLE") {
		t.Fatal("external import retained a destructive statement")
	}
}

func TestSanitizeMySQLImportRejectsClientGlobalAndExecutableStatements(t *testing.T) {
	for _, input := range []string{
		"DELIMITER $$\nCREATE PROCEDURE p() SELECT 1$$",
		"USE another_db;",
		"CREATE DATABASE another_db;",
		"CREATE USER 'attacker'@'%';",
		"SET GLOBAL general_log=ON;",
		"LOAD DATA INFILE '/tmp/x' INTO TABLE t;",
		"INSERT INTO other_db.t VALUES (1);",
		"INSERT INTO t VALUES (SLEEP(1));",
		"INSERT INTO t VALUES (qa_udf_exec('id'));",
		"INSERT INTO t SELECT * FROM source;",
		"CREATE TABLE t (id int) ENGINE=FEDERATED;",
		"CREATE TABLE t (id int) ENGINE=Spider;",
		"ALTER TABLE t ENGINE=Spider;",
		"CREATE TABLE t (id int, FULLTEXT KEY title_idx (id) WITH PARSER ngram);",
		"CREATE TRIGGER t AFTER INSERT ON x FOR EACH ROW SET @x=1;",
		"CREATE TABLE t (id int);\\! echo unsafe",
		"CREATE TABLE t (id varchar(10)",
		"CREATE TABLE t (id int))",
	} {
		t.Run(input, func(t *testing.T) {
			var output bytes.Buffer
			if err := sanitizeMySQLImport(context.Background(), strings.NewReader(input), &output); err == nil {
				t.Fatalf("unsafe/incomplete SQL was accepted: %s", output.String())
			}
		})
	}
}

func TestVisitSanitizedMySQLStatementsKeepsQuotedSemicolons(t *testing.T) {
	input := "INSERT INTO `items` (`label`) VALUES ('first;still first');\nCREATE TABLE `next` (`id` int);\n"
	var statements []string
	err := visitSanitizedMySQLStatements(context.Background(), strings.NewReader(input), func(statement string) error {
		statements = append(statements, statement)
		return nil
	})
	if err != nil || len(statements) != 2 || !strings.Contains(statements[0], "first;still first") || !strings.HasPrefix(statements[1], "CREATE TABLE") {
		t.Fatalf("sanitized SQL statement reader split or rejected a quoted semicolon: %#v, %v", statements, err)
	}
}

func TestPostgresImportUsesTypedJSONAndRejectsNativeArchivesAndSQL(t *testing.T) {
	input := `{"format":"powerlinux-postgresql-import-v1","tables":[{"name":"widgets","columns":[{"name":"id","type":"bigint"},{"name":"label","type":"text","nullable":true}],"primary_key":["id"],"rows":[[1,"x'); ALTER ROLE app PASSWORD 'owned"],[2,null]]}]}`
	prepared, err := parsePostgresJSONImport(context.Background(), bytes.NewReader([]byte(input)))
	if err != nil || len(prepared.Tables) != 1 || len(prepared.Tables[0].Rows) != 2 {
		t.Fatal("bounded typed PostgreSQL import was rejected", core.ErrorCode(err))
	}
	if prepared.Tables[0].Rows[0][1] != "x'); ALTER ROLE app PASSWORD 'owned" {
		t.Fatal("literal PostgreSQL text was not kept as a parameter value")
	}
	for name, invalid := range map[string]string{
		"custom archive": "PGDMP\x01\x0e\x00",
		"plain sql":      "CREATE TABLE public.widgets (id integer); ALTER ROLE app PASSWORD 'secret';",
		"unsafe type":    `{"format":"powerlinux-postgresql-import-v1","tables":[{"name":"widgets","columns":[{"name":"id","type":"integer; ALTER ROLE app SUPERUSER"}]}]}`,
		"system name":    `{"format":"powerlinux-postgresql-import-v1","tables":[{"name":"pg_authid","columns":[{"name":"id","type":"integer"}]}]}`,
		"unknown field":  `{"format":"powerlinux-postgresql-import-v1","sql":"ALTER ROLE app SUPERUSER","tables":[{"name":"widgets","columns":[{"name":"id","type":"integer"}]}]}`,
		"bad cell type":  `{"format":"powerlinux-postgresql-import-v1","tables":[{"name":"widgets","columns":[{"name":"id","type":"integer"}],"rows":[["1); ALTER ROLE app SUPERUSER"]]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePostgresJSONImport(context.Background(), bytes.NewReader([]byte(invalid))); err == nil {
				t.Fatal("non-typed or unsafe PostgreSQL import was accepted")
			}
		})
	}
}

func TestMongoImportArchiveIsFullyValidatedBeforeRestore(t *testing.T) {
	archive := func(t *testing.T, frames ...struct {
		kind byte
		doc  bson.D
	}) []byte {
		t.Helper()
		var output bytes.Buffer
		output.WriteString(mongoBackupMagic)
		for _, frame := range frames {
			raw, err := bson.Marshal(frame.doc)
			if err != nil {
				t.Fatal(err)
			}
			if err = mongoBackupFrame(&output, frame.kind, raw); err != nil {
				t.Fatal(err)
			}
		}
		return output.Bytes()
	}
	frame := func(kind byte, doc bson.D) struct {
		kind byte
		doc  bson.D
	} {
		return struct {
			kind byte
			doc  bson.D
		}{kind, doc}
	}
	valid := archive(t,
		frame('C', bson.D{{Key: "name", Value: "items"}, {Key: "type", Value: "collection"}, {Key: "options", Value: bson.D{}}}),
		frame('D', bson.D{{Key: "_id", Value: 1}, {Key: "value", Value: "safe"}}),
		frame('I', bson.D{{Key: "name", Value: "value_1"}, {Key: "key", Value: bson.D{{Key: "value", Value: 1}}}, {Key: "v", Value: 2}}),
	)
	if err := validateMongoImportArtifact(context.Background(), bytes.NewReader(valid)); err != nil {
		t.Fatal("valid collection/document/index archive was rejected", core.ErrorCode(err))
	}

	for name, invalid := range map[string][]byte{
		"trailing-unknown-frame": archive(t,
			frame('C', bson.D{{Key: "name", Value: "items"}, {Key: "type", Value: "collection"}}),
			frame('X', bson.D{{Key: "value", Value: 1}}),
		),
		"view":              archive(t, frame('C', bson.D{{Key: "name", Value: "view_items"}, {Key: "type", Value: "view"}})),
		"system-collection": archive(t, frame('C', bson.D{{Key: "name", Value: "system.users"}, {Key: "type", Value: "collection"}})),
		"nested-where": archive(t,
			frame('C', bson.D{{Key: "name", Value: "items"}, {Key: "type", Value: "collection"}}),
			frame('D', bson.D{{Key: "nested", Value: bson.A{bson.D{{Key: "$where", Value: "return true"}}}}}),
		),
		"nested-regex": archive(t,
			frame('C', bson.D{{Key: "name", Value: "items"}, {Key: "type", Value: "collection"}}),
			frame('D', bson.D{{Key: "nested", Value: bson.A{bson.Regex{Pattern: "(a+)+$"}}}}),
		),
		"unsafe-create-option":        archive(t, frame('C', bson.D{{Key: "name", Value: "items"}, {Key: "type", Value: "collection"}, {Key: "options", Value: bson.D{{Key: "create", Value: "system.users"}}}})),
		"unsafe-validator-function":   archive(t, frame('C', bson.D{{Key: "name", Value: "items"}, {Key: "type", Value: "collection"}, {Key: "options", Value: bson.D{{Key: "validator", Value: bson.D{{Key: "$expr", Value: bson.D{{Key: "$function", Value: bson.D{{Key: "body", Value: "return true"}}}}}}}}}})),
		"document-without-collection": archive(t, frame('D', bson.D{{Key: "_id", Value: 1}})),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateMongoImportArtifact(context.Background(), bytes.NewReader(invalid)); err == nil {
				t.Fatal("unsafe MongoDB archive was accepted")
			}
		})
	}

	// Malformed late frames must be rejected before restore attempts to connect
	// or clear any collection. The fixture has no live MongoDB endpoint.
	malformed := archive(t, frame('C', bson.D{{Key: "name", Value: "items"}, {Key: "type", Value: "collection"}}), frame('X', bson.D{}))
	e := &PrivilegedExecutor{}
	err := e.restoreMongoImportDatabase(context.Background(), Instance{PluginID: "mongodb"}, managedDatabaseRecord{}, bytes.NewReader(malformed))
	if core.ErrorCode(err) != "artifact_invalid" {
		t.Fatalf("restore did not fail in archive preflight: %v", err)
	}
}

func TestManagedDatabaseImportChunkCASAndExplicitInterruptedRecovery(t *testing.T) {
	e := testAgent(t)
	instance := Instance{ID: "mariadb-import-test", PluginID: "mariadb", Paths: Paths{BackupDir: t.TempDir()}}
	if err := realDirectory(instance.Paths.BackupDir); err != nil {
		t.Fatal(err)
	}
	record := managedDatabaseRecord{ManagedDatabase: ManagedDatabase{InstanceID: instance.ID, Database: "qa_import", Engine: instance.PluginID, Permission: "read_write", State: "active", Revision: 7}}
	payload := []byte("native dump")
	digest := sha256Hex(payload)
	started, err := e.beginManagedDatabaseImport(context.Background(), instance, record, ManagedDatabaseInput{ExpectedRevision: record.Revision, ImportBytes: int64(len(payload)), ImportSHA256: digest})
	if err != nil {
		t.Fatal("upload receipt could not be created", core.ErrorCode(err))
	}
	imported := started.(DatabaseImport)
	first := []byte("native ")
	progress, err := e.appendManagedDatabaseImportChunk(context.Background(), instance, record, ManagedDatabaseInput{ExpectedRevision: record.Revision, ImportID: imported.ID, ImportRevision: imported.Revision, ImportOffset: 0, ImportChunk: base64.StdEncoding.EncodeToString(first)})
	if err != nil {
		t.Fatal("first bounded chunk failed", core.ErrorCode(err))
	}
	progressImport := progress.(DatabaseImport)
	last := payload[len(first):]
	finished, err := e.appendManagedDatabaseImportChunk(context.Background(), instance, record, ManagedDatabaseInput{ExpectedRevision: record.Revision, ImportID: imported.ID, ImportRevision: progressImport.Revision, ImportOffset: int64(len(first)), ImportChunk: base64.StdEncoding.EncodeToString(last), ImportFinal: true})
	if err != nil {
		t.Fatal("final bounded chunk failed", core.ErrorCode(err))
	}
	finishedImport := finished.(DatabaseImport)
	if finishedImport.State != "uploaded" || finishedImport.BytesReceived != int64(len(payload)) || finishedImport.SHA256 != digest {
		t.Fatal("chunk upload did not produce a verified external import receipt")
	}
	if _, err = e.loadDatabaseImport(instance, "another_db", imported.ID); core.ErrorCode(err) != "forbidden" {
		t.Fatal("database import was visible through a different database resource")
	}
	currentRecord := record
	currentRecord.Revision++ // A credential or permission update does not orphan private artifacts.
	deleted, err := e.deleteManagedDatabaseImport(context.Background(), instance, currentRecord, imported.ID, finishedImport.Revision)
	if err != nil || deleted.(DatabaseImport).State != "deleted" {
		t.Fatal("current database owner could not delete an older-revision private import", core.ErrorCode(err))
	}

	// An apply task killed after its claim must be explicitly resolved. Recovery
	// marks uncertainty and never retries the database restore implicitly.
	interrupted := databaseImportReceipt{ID: "dbimport_0123456789abcdef0123456789abcdef", InstanceID: instance.ID, Database: record.Database, Engine: instance.PluginID, Filename: "dbimport_0123456789abcdef0123456789abcdef.external", ExpectedBytes: int64(len(payload)), BytesReceived: int64(len(payload)), ExpectedSHA256: digest, SHA256: digest, DatabaseRevision: record.Revision, Revision: 4, State: "applying"}
	if err = e.save("private", interrupted.ID, interrupted); err != nil {
		t.Fatal(err)
	}
	recovered, err := e.recoverManagedDatabaseImport(instance, currentRecord, interrupted.ID, interrupted.Revision)
	if err != nil {
		t.Fatal("interrupted import could not be explicitly resolved", core.ErrorCode(err))
	}
	resolved := recovered.(DatabaseImport)
	if resolved.State != "recovery_required" || resolved.Revision != interrupted.Revision+1 {
		t.Fatal("interrupted import did not enter the explicit recovery state")
	}
	if _, err = e.applyManagedDatabaseImport(context.Background(), instance, currentRecord, interrupted.ID, resolved.Revision); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("interrupted import was automatically replayable")
	}
}

func TestDatabaseImportHTTPStreamLeavesInterruptedUploadVisibleAndDeletable(t *testing.T) {
	e := testAgent(t)
	id := "mariadb-http-import"
	instance := Instance{ID: id, PluginID: "mariadb", InstanceID: id, Status: "installed", Paths: e.instancePaths("mariadb", id), Service: ServiceProfile{ID: id, Unit: "powerlinux-plugin-" + id + ".service"}}
	if err := os.MkdirAll(instance.Paths.BackupDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := e.save("receipts", id, instance); err != nil {
		t.Fatal(err)
	}
	record := managedDatabaseRecord{ManagedDatabase: ManagedDatabase{InstanceID: id, Database: "appdb", Engine: "mariadb", Username: "appuser", Permission: "read_write", State: "active", Revision: 9}, Credential: "not-used-during-upload"}
	if err := e.save("private", managedDatabaseKey(id, record.Database), record); err != nil {
		t.Fatal(err)
	}
	payload := []byte("a larger external SQL dump")
	input := databaseImportStreamInput{InstanceID: id, Database: record.Database, ImportID: "dbimport_0123456789abcdef0123456789abcdef", ExpectedBytes: int64(len(payload)), ExpectedSHA256: sha256Hex(payload), DatabaseRevision: record.Revision}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	partial := payload[:8]
	_, err = e.databaseImportArtifactStream(context.Background(), agent.Request{Operation: "database_import_artifact", Input: raw}, agent.IO{Stdin: io.NopCloser(strings.NewReader(string(partial)))})
	if core.ErrorCode(err) != "artifact_transfer_failed" {
		t.Fatalf("short upload did not fail safely: %v", err)
	}
	receipt, err := e.loadDatabaseImport(instance, record.Database, input.ImportID)
	if err != nil || receipt.State != "uploading" || receipt.BytesReceived != int64(len(partial)) || receipt.Revision != 2 {
		t.Fatalf("interrupted upload receipt was not preserved: state=%q bytes=%d revision=%d err=%v", receipt.State, receipt.BytesReceived, receipt.Revision, err)
	}
	if _, err = os.Stat(filepath.Join(instance.Paths.BackupDir, databaseImportTempName(input.ImportID))); err != nil {
		t.Fatalf("interrupted upload file was not left for explicit cleanup: %v", err)
	}
	deleted, err := e.deleteManagedDatabaseImport(context.Background(), instance, record, input.ImportID, receipt.Revision)
	if err != nil || deleted.(DatabaseImport).State != "deleted" {
		t.Fatalf("interrupted upload could not be explicitly deleted: %v", err)
	}
	if _, err = os.Stat(filepath.Join(instance.Paths.BackupDir, databaseImportTempName(input.ImportID))); !os.IsNotExist(err) {
		t.Fatalf("explicit deletion left the interrupted upload behind: %v", err)
	}
}
