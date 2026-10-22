package plugins

import "testing"

func TestFirstReleaseDatabaseCatalogSupportsUbuntu2404AMD64(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	ubuntu := Platform{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "24.04"}
	unsupported := []Platform{
		{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "22.04"},
		{OS: "linux", Arch: "arm64", Distro: "ubuntu", Release: "24.04"},
	}
	for _, id := range []string{"mysql", "mysql-community", "postgresql"} {
		manifest, err := catalog.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !manifest.Supports(ubuntu) {
			t.Errorf("%s does not support first-release target %+v", id, ubuntu)
		}
		for _, platform := range unsupported {
			if manifest.Supports(platform) {
				t.Errorf("%s unexpectedly supports unreviewed target %+v", id, platform)
			}
		}
	}
}

func TestDatabaseWebToolsHaveTrustedSourcesAndManagedLoopbackServices(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	pma, err := catalog.Get("phpmyadmin")
	if err != nil {
		t.Fatal(err)
	}
	if len(pma.Versions) != 1 || pma.Versions[0].Source.Kind != "verified_archive" || pma.Versions[0].Source.SHA256 != "03de2640bb25c9a6f96bc94eae316080b5fd5bd58d769e1318ba9dd94c83364c" || pma.Service.Programs[0] != "php" || pma.Service.DefaultPort != 19050 {
		t.Fatalf("phpMyAdmin does not use its pinned upstream artifact and managed PHP service: %+v", pma)
	}
	if pma.Verification.Status != "not_tested" {
		t.Fatalf("phpMyAdmin claims live acceptance without host evidence: %+v", pma.Verification)
	}
	pg, err := catalog.Get("pgadmin")
	if err != nil {
		t.Fatal(err)
	}
	source := pg.Versions[0].Source
	if source.Kind != "signed_package" || len(source.Packages) != 1 || source.Packages[0] != "pgadmin4-server" || source.RepositoryKeySHA256 != "546cbd50f8cadcf0832dc0aef9959b1ccf0b7e9a5e99c1518784bcc46f455585" || pg.Service.DefaultPort != 19051 || pg.Service.Programs[0] != "python3" {
		t.Fatalf("pgAdmin does not use its pinned vendor key and isolated server package: %+v", pg)
	}
	// Validate both supported Debian feeds explicitly; the test runner itself
	// may be Ubuntu and must not be mistaken for a Debian release number.
	for release, suite := range map[string]string{"12": "bookworm", "13": "trixie"} {
		candidate, err := BuiltinCatalog()
		if err != nil {
			t.Fatal(err)
		}
		candidate.useDatabaseWebToolSources(Platform{OS: "linux", Arch: "amd64", Distro: "debian", Release: release})
		tool, err := candidate.Get("pgadmin")
		if err != nil {
			t.Fatal(err)
		}
		if tool.Versions[0].Source.Repository != "https://ftp.postgresql.org/pub/pgadmin/pgadmin4/apt/"+suite {
			t.Fatalf("pgAdmin repository does not match Debian %s: %s", release, tool.Versions[0].Source.Repository)
		}
	}
	for _, item := range []Manifest{pma, pg} {
		if item.Verification.Status != "not_tested" {
			t.Errorf("%s claims unperformed host acceptance", item.ID)
		}
		if len(item.Capabilities) == 0 {
			t.Errorf("%s lost its legacy capability ledger", item.ID)
		}
	}
	if pg.Supports(Platform{OS: "linux", Arch: "arm64", Distro: "debian", Release: "13"}) {
		t.Fatal("pgAdmin advertised an unsupported official-repository architecture")
	}
}

func TestSelectedRuntimeCatalogIsVerifiedOnlyOnUbuntu2404AMD64(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	ubuntu := Platform{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "24.04"}
	for _, id := range []string{"git", "nodejs", "golang"} {
		manifest, err := catalog.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Verification.Status != "verified" || !manifest.Supports(ubuntu) {
			t.Errorf("%s lacks Ubuntu 24.04 amd64 verification: %+v", id, manifest.Verification)
		}
		for _, platform := range []Platform{
			{OS: "linux", Arch: "amd64", Distro: "debian", Release: "13"},
			{OS: "linux", Arch: "arm64", Distro: "ubuntu", Release: "24.04"},
		} {
			if manifest.Supports(platform) {
				t.Errorf("%s unexpectedly supports unaccepted platform %+v", id, platform)
			}
		}
	}
}
