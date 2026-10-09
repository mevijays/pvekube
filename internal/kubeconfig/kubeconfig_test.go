package kubeconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type pki struct{ caB64, certB64, keyB64 string }

// newPKI makes a CA plus a client cert/key signed by it, valid from before to
// after, so tests can build credentials that are fresh, expired or expiring.
func newPKI(t *testing.T, before, after time.Time) pki {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: before, NotAfter: after, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "admin"},
		NotBefore: before, NotAfter: after, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	enc := func(typ string, b []byte) string {
		return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}))
	}
	return pki{caB64: enc("CERTIFICATE", caDER), certB64: enc("CERTIFICATE", der), keyB64: enc("EC PRIVATE KEY", keyDER)}
}

func fresh(t *testing.T) pki {
	return newPKI(t, time.Now().Add(-time.Hour), time.Now().Add(365*24*time.Hour))
}

// kubeadmStyle builds a kubeconfig the way kubeadm names things: every cluster
// is "kubernetes", the user "kubernetes-admin".
func kubeadmStyle(p pki, server string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: kubernetes
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: kubernetes-admin
  user:
    client-certificate-data: %s
    client-key-data: %s
contexts:
- name: kubernetes-admin@kubernetes
  context:
    cluster: kubernetes
    user: kubernetes-admin
current-context: kubernetes-admin@kubernetes
`, server, p.caB64, p.certB64, p.keyB64)
}

func findings(rep Report) []Finding {
	out := append([]Finding(nil), rep.Global...)
	for _, f := range rep.Files {
		out = append(out, f.Findings...)
	}
	return out
}

func has(rep Report, level Level, substr string) bool {
	for _, f := range findings(rep) {
		if f.Level == level && strings.Contains(f.Msg, substr) {
			return true
		}
	}
	return false
}

func TestSameNamedKubeadmClustersConflictByDefault(t *testing.T) {
	a := Source{"prod.yaml", kubeadmStyle(fresh(t), "https://10.0.0.1:6443")}
	b := Source{"dev.yaml", kubeadmStyle(fresh(t), "https://10.0.0.2:6443")}

	rep, out := Merge([]Source{a, b}, Options{})
	if rep.CanDownload || out != nil {
		t.Fatal("a name clash with different contents must block generating a file")
	}
	// Cluster and user are the real clashes; the context that joins them is
	// identical in both files, so it is not a separate conflict.
	for _, kind := range []string{"cluster", "user"} {
		if !has(rep, Error, kind+` "`) || !has(rep, Error, "different settings") {
			t.Errorf("no conflict error for %s: %+v", kind, findings(rep))
		}
	}
	// The clash is the root cause; it must not also be reported as a dangling
	// reference to something that "is not defined".
	if has(rep, Error, "not defined in any uploaded file") {
		t.Errorf("clash produced a misleading dangling-reference error: %+v", findings(rep))
	}
}

func TestRenameKeepsBothClustersAndTheirOwnCredentials(t *testing.T) {
	pa, pb := fresh(t), fresh(t)
	a := Source{"prod.yaml", kubeadmStyle(pa, "https://10.0.0.1:6443")}
	b := Source{"dev.yaml", kubeadmStyle(pb, "https://10.0.0.2:6443")}

	rep, out := Merge([]Source{a, b}, Options{OnConflict: OnConflictRename})
	if !rep.CanDownload {
		t.Fatalf("rename should resolve the clash: %+v", findings(rep))
	}
	var got struct {
		Clusters []struct {
			Name    string
			Cluster map[string]any
		}
		Users []struct {
			Name string
			User map[string]any
		}
		Contexts []struct {
			Name    string
			Context map[string]string
		}
		CurrentContext string `yaml:"current-context"`
	}
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("merged output is not valid YAML: %v", err)
	}
	if len(got.Clusters) != 2 || len(got.Users) != 2 || len(got.Contexts) != 2 {
		t.Fatalf("want 2/2/2, got %d/%d/%d", len(got.Clusters), len(got.Users), len(got.Contexts))
	}
	if got.CurrentContext != "kubernetes-admin@kubernetes" {
		t.Errorf("current-context = %q, want the first file's", got.CurrentContext)
	}

	servers := map[string]string{}
	for _, c := range got.Clusters {
		servers[c.Name] = c.Cluster["server"].(string)
	}
	userCert := map[string]string{}
	for _, u := range got.Users {
		userCert[u.Name] = u.User["client-certificate-data"].(string)
	}
	// Every context must resolve, and must resolve to ITS OWN file's cluster
	// and credentials — the whole point of renaming rather than dropping.
	want := map[string]struct{ server, cert string }{
		"kubernetes-admin@kubernetes":     {"https://10.0.0.1:6443", pa.certB64},
		"kubernetes-admin@kubernetes-dev": {"https://10.0.0.2:6443", pb.certB64},
	}
	for _, c := range got.Contexts {
		w, ok := want[c.Name]
		if !ok {
			t.Errorf("unexpected context %q", c.Name)
			continue
		}
		if servers[c.Context["cluster"]] != w.server {
			t.Errorf("context %q points at server %q, want %q", c.Name, servers[c.Context["cluster"]], w.server)
		}
		if userCert[c.Context["user"]] != w.cert {
			t.Errorf("context %q uses another file's credentials", c.Name)
		}
	}
	if !has(rep, Warning, "renamed to") {
		t.Error("renames should be surfaced as warnings")
	}
}

func TestIdenticalDuplicatesAreMergedNotRenamed(t *testing.T) {
	p := fresh(t)
	src := kubeadmStyle(p, "https://10.0.0.1:6443")
	rep, out := Merge([]Source{{"one.yaml", src}, {"two.yaml", src}}, Options{})
	if !rep.CanDownload {
		t.Fatalf("identical files should merge cleanly: %+v", findings(rep))
	}
	var got struct{ Clusters, Users, Contexts []any }
	_ = yaml.Unmarshal(out, &got)
	if len(got.Clusters) != 1 || len(got.Users) != 1 || len(got.Contexts) != 1 {
		t.Errorf("want one of each, got %d/%d/%d", len(got.Clusters), len(got.Users), len(got.Contexts))
	}
}

func TestCredentialProblemsAreErrors(t *testing.T) {
	good := fresh(t)
	expired := newPKI(t, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	soon := newPKI(t, time.Now().Add(-time.Hour), time.Now().Add(10*24*time.Hour))
	other := fresh(t)

	cases := []struct {
		name   string
		src    string
		level  Level
		substr string
	}{
		{"expired client cert", kubeadmStyle(expired, "https://h:6443"), Error, "expired on"},
		{"cert expiring soon", kubeadmStyle(soon, "https://h:6443"), Warning, "within 30 days"},
		{"cert does not match key", strings.Replace(kubeadmStyle(good, "https://h:6443"), good.keyB64, other.keyB64, 1), Error, "do not match"},
		{"certificate without key", strings.Replace(kubeadmStyle(good, "https://h:6443"), "    client-key-data: "+good.keyB64+"\n", "", 1), Error, "no client-key-data"},
		{"garbage base64 CA", strings.Replace(kubeadmStyle(good, "https://h:6443"), good.caB64, `"!!!not-base64!!!"`, 1), Error, "certificate-authority-data is invalid"},
		{"missing server", strings.Replace(kubeadmStyle(good, "https://h:6443"), "server: https://h:6443", "server: \"\"", 1), Error, "no server address"},
		{"bad server scheme", kubeadmStyle(good, "ftp://h:6443"), Error, "must start with https://"},
		{"plain http server", kubeadmStyle(good, "http://h:6443"), Warning, "plain http"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, _ := Merge([]Source{{"f.yaml", tc.src}}, Options{})
			if !has(rep, tc.level, tc.substr) {
				t.Errorf("want %s containing %q, got %+v", tc.level, tc.substr, findings(rep))
			}
			if tc.level == Error && rep.CanDownload {
				t.Error("an error must block the download")
			}
		})
	}
}

func TestStructuralProblems(t *testing.T) {
	good := kubeadmStyle(fresh(t), "https://h:6443")
	cases := []struct {
		name   string
		src    string
		substr string
	}{
		{"empty", "  \n", "empty"},
		{"not yaml", "clusters: [unclosed", "not valid YAML"},
		{"wrong kind", "apiVersion: v1\nkind: Pod\nmetadata: {name: x}\n", `expected "Config"`},
		{"nothing in it", "apiVersion: v1\nkind: Config\n", "nothing to merge"},
		{"context without cluster", strings.Replace(good, "    cluster: kubernetes\n", "", 1), `has no cluster`},
		{"dangling cluster", strings.Replace(good, "cluster: kubernetes\n", "cluster: nope\n", 1), `refers to cluster "nope"`},
		{"dangling user", strings.Replace(good, "user: kubernetes-admin\n", "user: nope\n", 1), `refers to user "nope"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, out := Merge([]Source{{"f.yaml", tc.src}}, Options{})
			if !has(rep, Error, tc.substr) {
				t.Errorf("want an error containing %q, got %+v", tc.substr, findings(rep))
			}
			if rep.CanDownload || out != nil {
				t.Error("must not produce a file")
			}
		})
	}
}

// A dangling reference in one file is fine when another file supplies it.
func TestContextCanUseClusterFromAnotherFile(t *testing.T) {
	p := fresh(t)
	full := kubeadmStyle(p, "https://h:6443")
	onlyContext := "apiVersion: v1\nkind: Config\ncontexts:\n- name: alias\n  context: {cluster: kubernetes, user: kubernetes-admin}\n"
	rep, _ := Merge([]Source{{"full.yaml", full}, {"alias.yaml", onlyContext}}, Options{})
	if !rep.CanDownload {
		t.Errorf("cross-file reference should resolve: %+v", findings(rep))
	}
}

func TestNonPortableAndRiskySettingsWarn(t *testing.T) {
	src := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://h:6443
    certificate-authority: /home/me/ca.crt
- name: insecure
  cluster:
    server: https://h2:6443
    insecure-skip-tls-verify: true
users:
- name: pathuser
  user:
    client-certificate: /home/me/c.crt
    client-key: /home/me/c.key
- name: execuser
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
- name: nocreds
  user: {}
contexts:
- name: x
  context: {cluster: c, user: pathuser}
- name: y
  context: {cluster: insecure, user: execuser}
- name: z
  context: {cluster: c, user: nocreds}
`
	rep, _ := Merge([]Source{{"f.yaml", src}}, Options{})
	for _, want := range []string{"certificate-authority is a file path", "skips TLS verification", "file paths", `runs the command "aws"`, "has no credentials"} {
		if !has(rep, Warning, want) {
			t.Errorf("missing warning containing %q: %+v", want, findings(rep))
		}
	}
	// Warnings alone must not block a download.
	if !rep.CanDownload {
		t.Errorf("warnings should not block: %+v", findings(rep))
	}
}

func TestCurrentContextSelection(t *testing.T) {
	p := fresh(t)
	a := Source{"a.yaml", kubeadmStyle(p, "https://h:6443")}
	b := Source{"b.yaml", strings.NewReplacer("kubernetes", "other").Replace(kubeadmStyle(fresh(t), "https://h2:6443"))}

	rep, out := Merge([]Source{a, b}, Options{CurrentContext: "other-admin@other"})
	if !rep.CanDownload || rep.CurrentContext != "other-admin@other" || !strings.Contains(string(out), "current-context: other-admin@other") {
		t.Errorf("chosen current-context not applied: %q %+v", rep.CurrentContext, findings(rep))
	}

	rep, _ = Merge([]Source{a, b}, Options{CurrentContext: "does-not-exist"})
	if rep.CanDownload || !has(rep, Error, "does not exist") {
		t.Errorf("an unknown current context must be an error: %+v", findings(rep))
	}
}

func TestFindingsNeverContainSecrets(t *testing.T) {
	p := newPKI(t, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	rep, _ := Merge([]Source{{"f.yaml", kubeadmStyle(p, "https://h:6443")}}, Options{})
	for _, f := range findings(rep) {
		for _, secret := range []string{p.keyB64, p.certB64, p.caB64} {
			if strings.Contains(f.Msg, secret) {
				t.Fatalf("a finding leaked credential material: %s", f.Msg)
			}
		}
	}
}

func TestNoSources(t *testing.T) {
	rep, out := Merge(nil, Options{})
	if rep.CanDownload || out != nil || rep.Errors == 0 {
		t.Errorf("no files must be an error: %+v", rep)
	}
}
