package sites

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func basicPasswordHash(password string) (string, error) {
	data, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(data), err
}

func (e *AgentExecutor) certificate(id string) (Certificate, error) {
	var value Certificate
	if !safeID.MatchString(id) {
		return value, invalid("Invalid certificate ID")
	}
	err := rootJSON(e.state, filepath.Join("certificates", id+".json"), &value)
	if errors.Is(err, fs.ErrNotExist) {
		return value, failure("not_found", "Certificate was not found", 404)
	}
	if err != nil {
		return value, err
	}
	if value.ID != id {
		return value, errors.New("certificate receipt identity mismatch")
	}
	return value, nil
}
func (e *AgentExecutor) certificates() ([]Certificate, error) {
	dir, err := e.state.Open("certificates")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := []Certificate{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		cert, err := e.certificate(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
		if len(out) > 10000 {
			return nil, failure("too_many_certificates", "Certificate inventory exceeds its limit", 413)
		}
	}
	return out, nil
}
func (e *AgentExecutor) importCertificate(ctx context.Context, in request) (meta Certificate, err error) {
	if in.Certificate == nil || in.Certificate.ID != in.ID {
		return Certificate{}, invalid("Certificate metadata is required")
	}
	meta, err = inspectCertificate(in.ID, in.Certificate.Name, in.CertPEM, in.KeyPEM)
	if err != nil {
		return meta, err
	}
	prior, getErr := e.certificate(in.ID)
	if getErr != nil && !isNotFound(getErr) {
		return meta, getErr
	}
	if getErr == nil && prior.Revision != in.ExpectedRevision || isNotFound(getErr) && in.ExpectedRevision != 0 {
		return meta, failure("revision_conflict", "Certificate revision changed", 412)
	}
	all, err := e.list()
	if err != nil {
		return meta, err
	}
	references := []Site{}
	runtimeIDs := map[string]bool{}
	block, _ := pem.Decode([]byte(in.CertPEM))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return meta, err
	}
	for _, site := range all {
		if site.TLS.Enabled && site.TLS.CertificateID == in.ID {
			if in.ACME == nil || !in.ACME.Renew || !slices.Contains(in.AuthorizedSites, site.ID) {
				return meta, failure("certificate_in_use", "Active certificate replacement requires explicit renewal authorization for every bound website", 409)
			}
			for _, value := range allSiteDomains(site) {
				if !certificateCovers(leaf, value) {
					return meta, failure("certificate_name_mismatch", "Renewed certificate does not cover every bound website domain", 409)
				}
			}
			references = append(references, site)
			runtimeIDs[site.WebInstance] = true
		}
	}
	meta.Revision = in.ExpectedRevision + 1
	meta.Source = in.Certificate.Source
	if meta.Source == "" {
		meta.Source = "uploaded"
	}
	meta.UpdatedAt = time.Now().UTC()
	changes := []changedFile{}
	committed := false
	defer func() {
		if committed {
			return
		}
		recoverErr := rollbackConfigs(changes)
		recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for id := range runtimeIDs {
			recoverErr = errors.Join(recoverErr, e.runtimes.Reload(recoverCtx, id))
		}
		if recoverErr != nil {
			err = errors.Join(err, failure("rollback_incomplete", "Certificate renewal requires recovery inspection", 500))
		}
	}()
	for _, extension := range []string{"crt", "key"} {
		data := map[string][]byte{"crt": []byte(in.CertPEM), "key": []byte(in.KeyPEM)}[extension]
		path := filepath.Join(e.config.StateDir, "certificates", in.ID+"."+extension)
		before, err := snapshotConfig(path)
		if err != nil {
			return meta, err
		}
		changes = append(changes, before)
		if err = writeProtected(path, data); err != nil {
			return meta, err
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for id := range runtimeIDs {
		if err = e.runtimes.Check(checkCtx, id); err != nil {
			return meta, failure("configuration_rejected", "Web runtime rejected the renewed certificate", 409)
		}
	}
	for id := range runtimeIDs {
		if err = e.runtimes.Reload(checkCtx, id); err != nil {
			return meta, failure("reload_failed", "Web runtime did not accept the renewed certificate", 500)
		}
	}
	if verifier, ok := e.runtimes.(RuntimeVerifier); ok {
		for _, site := range references {
			if err = verifier.VerifySite(checkCtx, site); err != nil {
				return meta, failure("runtime_not_applied", "Web runtime did not serve the renewed certificate; protected files were restored", 502)
			}
		}
	}
	// Publish metadata only after the running service has served the exact new
	// certificate. Readers never see a new revision for a rejected renewal.
	publish := func(path string, data []byte) error {
		before, snapshotErr := snapshotConfig(path)
		if snapshotErr != nil {
			return snapshotErr
		}
		changes = append(changes, before)
		return writeProtected(path, data)
	}
	if err = publish(filepath.Join(e.config.StateDir, "certificates", in.ID+".json"), encode(meta)); err != nil {
		return meta, err
	}
	for _, site := range references {
		site.TLS.ExpiresAt = meta.ExpiresAt.Format(time.RFC3339)
		// The certificate has its own CAS revision; derived fields do not
		// change the generation of the website's unchanged configuration.
		site.TLS.CertificateRevision = meta.Revision
		site.UpdatedAt = meta.UpdatedAt
		site.UpdatedBy = in.Actor.Public()
		if err = publish(filepath.Join(e.config.StateDir, "sites", site.ID+".json"), encode(site)); err != nil {
			return meta, err
		}
	}
	committed = true
	return meta, nil
}
func certificateCovers(leaf *x509.Certificate, value string) bool {
	_, host, _, err := domain(value)
	if err != nil {
		return false
	}
	if strings.HasPrefix(host, "*.") {
		for _, name := range leaf.DNSNames {
			if strings.EqualFold(name, host) {
				return true
			}
		}
		return false
	}
	return leaf.VerifyHostname(host) == nil
}
func (e *AgentExecutor) exportCertificate(id string) (any, error) {
	meta, err := e.certificate(id)
	if err != nil {
		return nil, err
	}
	cert, err := e.state.ReadFile(filepath.Join("certificates", id+".crt"))
	if err != nil {
		return nil, err
	}
	key, err := e.state.ReadFile(filepath.Join("certificates", id+".key"))
	if err != nil {
		return nil, err
	}
	if len(cert) > 64<<10 || len(key) > 16<<10 {
		return nil, failure("too_large", "Certificate material exceeds its export limit", 413)
	}
	return map[string]any{"certificate": meta, "cert_pem": string(cert), "key_pem": string(key)}, nil
}
func (e *AgentExecutor) deleteCertificate(in request) (any, error) {
	cert, err := e.certificate(in.ID)
	if err != nil {
		return nil, err
	}
	if cert.Revision != in.ExpectedRevision {
		return nil, failure("revision_conflict", "Certificate revision changed", 412)
	}
	all, err := e.list()
	if err != nil {
		return nil, err
	}
	for _, site := range all {
		if site.TLS.CertificateID == in.ID {
			return nil, failure("certificate_in_use", "Remove all site references before deleting this certificate", 409)
		}
	}
	// Remove its inventory record last so an interrupted delete remains a
	// visible repairable object rather than inaccessible private key residue.
	for _, extension := range []string{"key", "crt", "json"} {
		if err = e.state.Remove(filepath.Join("certificates", in.ID+"."+extension)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return map[string]any{"id": in.ID, "deleted": true}, nil
}
