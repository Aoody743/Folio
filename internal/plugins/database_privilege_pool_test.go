package plugins

import (
	"context"
	"database/sql/driver"
	"fmt"
	"testing"
	"time"
)

func TestMySQLPrivilegeVerificationWorksWithSingleConnectionImportPool(t *testing.T) {
	for _, partial := range []string{"ON", "OFF"} {
		t.Run(partial, func(t *testing.T) {
			database := "app_data"
			grantDatabase := database
			if partial == "OFF" {
				grantDatabase = `app\_data`
			}
			fixture := &replicationSQLFixture{query: func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
				switch query {
				case "SHOW GLOBAL VARIABLES LIKE 'partial_revokes'":
					return []string{"Variable_name", "Value"}, [][]driver.Value{{"partial_revokes", partial}}, nil
				case "SHOW GRANTS FOR CURRENT_USER":
					return []string{"Grants"}, [][]driver.Value{{"GRANT USAGE ON *.* TO 'app_user'@'localhost'"}, {"GRANT ALL PRIVILEGES ON `" + grantDatabase + "`.* TO 'app_user'@'localhost'"}}, nil
				default:
					return nil, nil, fmt.Errorf("unexpected privilege query: %s", query)
				}
			}}
			db := openReplicationSQLFixture(t, fixture)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := verifySQLLeastPrivilege(ctx, db, DatabaseConnection{Engine: "mysql", Database: database}); err != nil {
				t.Fatal("single-connection privilege check deadlocked or rejected its scoped grant", err)
			}
			if db.Stats().WaitCount != 0 {
				t.Fatal("privilege verification tried to acquire a second connection while holding rows")
			}
		})
	}
}
