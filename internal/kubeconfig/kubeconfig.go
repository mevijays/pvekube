// Package kubeconfig validates and merges kubeconfig files.
//
// It exists for the common chore of combining the admin kubeconfigs of several
// clusters into one file you can switch between. Doing that by hand is
// error-prone for a reason worth designing around: tools like kubeadm give
// EVERY cluster the same names ("kubernetes", "kubernetes-admin",
// "kubernetes-admin@kubernetes"), so a naive merge silently keeps one cluster's
// credentials and drops the other's — and `kubectl config view --flatten`
// does exactly that ("first file wins") without telling you.
//
// So Merge reports problems up front instead of producing a file that merely
// looks right: unparsable input, certificates that are expired or do not match
// their key, contexts that point at a cluster or user nobody defined, entries
// with the same name but different contents, and settings that will not work
// on another machine (file paths). Nothing here touches disk or the network,
// and file contents never appear in a finding — only names.
package kubeconfig

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Source is one uploaded file.
type Source struct {
	Name    string
	Content string
}

// ConflictStrategy says what to do when two files define an entry with the
// same name but different contents.
type ConflictStrategy string

const (
	// OnConflictError reports the clash as an error and produces no file, so
	// the operator decides. The default: silently choosing is how credentials
	// get lost.
	OnConflictError ConflictStrategy = "error"
	// OnConflictRename keeps both by renaming the later one with its file's
	// name as a suffix, and rewrites the contexts that referred to it.
	OnConflictRename ConflictStrategy = "rename"
)

// Options tunes Merge.
type Options struct {
	OnConflict ConflictStrategy
	// CurrentContext, if set, becomes the merged file's current-context. It
	// must exist after merging. Empty means "the first file's, if it has one".
	CurrentContext string
}

// Level is a finding's severity. Only errors block generating a file.
type Level string

const (
	Error   Level = "error"
	Warning Level = "warning"
	Info    Level = "info"
)

// Finding is one validation result. File is empty for cross-file findings.
type Finding struct {
	Level Level
	File  string
	Msg   string
}

// FileReport summarises one uploaded file.
type FileReport struct {
	Name                      string
	Clusters, Users, Contexts int
	Findings                  []Finding
	// Status is the worst level among Findings: "error", "warning" or "ok".
	Status string
}

// ContextRow is one context of the merged result, for display. It carries no
// credentials.
type ContextRow struct {
	Name    string
	Cluster string
	Server  string
	User    string
	Auth    string // human description of how the user authenticates
	Source  string // file it came from
}

// Report is the outcome of validating (and, if clean, merging) a set of files.
type Report struct {
	Files          []FileReport
	Global         []Finding
	Contexts       []ContextRow
	ContextNames   []string
	CurrentContext string
	Errors         int
	Warnings       int
	// CanDownload is true when there are no errors and at least one context.
	CanDownload bool
}

// now is a variable so tests can pin the clock for certificate expiry.
var now = time.Now

const certExpirySoon = 30 * 24 * time.Hour

type named struct {
	name   string
	body   map[string]any
	source string
}

type parsedFile struct {
	name     string
	clusters []named
	users    []named
	contexts []named
	current  string
	findings []Finding
	// idx is this file's position in Report.Files.
	idx int
}

type rawFile struct {
	APIVersion     string         `yaml:"apiVersion"`
	Kind           string         `yaml:"kind"`
	Preferences    map[string]any `yaml:"preferences"`
	Extensions     any            `yaml:"extensions"`
	CurrentContext string         `yaml:"current-context"`
	Clusters       []struct {
		Name    string         `yaml:"name"`
		Cluster map[string]any `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string         `yaml:"name"`
		User map[string]any `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string         `yaml:"name"`
		Context map[string]any `yaml:"context"`
	} `yaml:"contexts"`
}

// Merge validates every source and, when nothing is wrong, returns the merged
// kubeconfig. The returned bytes are nil unless Report.CanDownload.
func Merge(sources []Source, opt Options) (Report, []byte) {
	if opt.OnConflict == "" {
		opt.OnConflict = OnConflictError
	}
	var rep Report
	if len(sources) == 0 {
		rep.Global = append(rep.Global, Finding{Level: Error, Msg: "no kubeconfig files were provided"})
		rep.count()
		return rep, nil
	}

	files := make([]*parsedFile, 0, len(sources))
	for i, src := range sources {
		pf := parseFile(src)
		pf.idx = i
		files = append(files, pf)
		rep.Files = append(rep.Files, FileReport{
			Name: src.Name, Findings: pf.findings,
			Clusters: len(pf.clusters), Users: len(pf.users), Contexts: len(pf.contexts),
		})
	}

	merged := mergeFiles(files, opt, &rep)
	finish(&rep, merged, opt)

	var out []byte
	if rep.CanDownload {
		out = merged.marshal(rep.CurrentContext)
	}
	return rep, out
}

// ---- parsing & per-file validation ----------------------------------------

func (pf *parsedFile) add(l Level, format string, args ...any) {
	pf.findings = append(pf.findings, Finding{Level: l, File: pf.name, Msg: fmt.Sprintf(format, args...)})
}

func parseFile(src Source) *parsedFile {
	pf := &parsedFile{name: src.Name}
	if strings.TrimSpace(src.Content) == "" {
		pf.add(Error, "the file is empty")
		return pf
	}

	var raw rawFile
	dec := yaml.NewDecoder(strings.NewReader(src.Content))
	if err := dec.Decode(&raw); err != nil {
		pf.add(Error, "not valid YAML/JSON: %v", err)
		return pf
	}
	var extra rawFile
	if dec.Decode(&extra) == nil {
		pf.add(Warning, "the file has more than one YAML document; only the first is used")
	}

	switch {
	case raw.Kind == "":
		pf.add(Warning, `"kind" is missing (expected Config)`)
	case raw.Kind != "Config":
		pf.add(Error, `kind is %q, expected "Config" — this does not look like a kubeconfig`, raw.Kind)
		return pf
	}
	if raw.APIVersion != "v1" {
		pf.add(Warning, `apiVersion is %q (expected v1)`, raw.APIVersion)
	}
	if raw.Extensions != nil || len(raw.Preferences) > 0 {
		pf.add(Info, `top-level "preferences"/"extensions" are not carried into the merged file`)
	}
	if len(raw.Clusters)+len(raw.Users)+len(raw.Contexts) == 0 {
		pf.add(Error, "contains no clusters, users or contexts — nothing to merge")
		return pf
	}

	seen := map[string]map[string]bool{"cluster": {}, "user": {}, "context": {}}
	check := func(kind, name string) bool {
		if strings.TrimSpace(name) == "" {
			pf.add(Error, "a %s has no name", kind)
			return false
		}
		if seen[kind][name] {
			pf.add(Error, "%s %q is defined more than once in this file", kind, name)
			return false
		}
		seen[kind][name] = true
		return true
	}
	for _, c := range raw.Clusters {
		if !check("cluster", c.Name) {
			continue
		}
		pf.clusters = append(pf.clusters, named{name: c.Name, body: orEmpty(c.Cluster), source: src.Name})
		validateCluster(pf, c.Name, orEmpty(c.Cluster))
	}
	for _, u := range raw.Users {
		if !check("user", u.Name) {
			continue
		}
		pf.users = append(pf.users, named{name: u.Name, body: orEmpty(u.User), source: src.Name})
		validateUser(pf, u.Name, orEmpty(u.User))
	}
	for _, c := range raw.Contexts {
		if !check("context", c.Name) {
			continue
		}
		pf.contexts = append(pf.contexts, named{name: c.Name, body: orEmpty(c.Context), source: src.Name})
		if str(c.Context, "cluster") == "" {
			pf.add(Error, "context %q has no cluster", c.Name)
		}
		if str(c.Context, "user") == "" {
			pf.add(Error, "context %q has no user", c.Name)
		}
	}
	pf.current = raw.CurrentContext
	return pf
}

func validateCluster(pf *parsedFile, name string, b map[string]any) {
	server := str(b, "server")
	if server == "" {
		pf.add(Error, "cluster %q has no server address", name)
	} else if u, err := url.Parse(server); err != nil || u.Host == "" {
		pf.add(Error, "cluster %q: server %q is not a valid URL", name, server)
	} else if u.Scheme != "https" && u.Scheme != "http" {
		pf.add(Error, "cluster %q: server %q must start with https:// (or http://)", name, server)
	} else if u.Scheme == "http" {
		pf.add(Warning, "cluster %q: server uses plain http — traffic and credentials are not encrypted", name)
	}

	ca, caPath := str(b, "certificate-authority-data"), str(b, "certificate-authority")
	switch {
	case ca != "":
		certs, err := parseCerts(ca)
		if err != nil {
			pf.add(Error, "cluster %q: certificate-authority-data is invalid: %v", name, err)
		} else {
			for _, c := range certs {
				if now().After(c.NotAfter) {
					pf.add(Error, "cluster %q: the CA certificate expired on %s", name, c.NotAfter.Format("2006-01-02"))
				}
			}
		}
		if caPath != "" {
			pf.add(Warning, "cluster %q sets both certificate-authority and certificate-authority-data; the inline data wins", name)
		}
	case caPath != "":
		pf.add(Warning, "cluster %q: certificate-authority is a file path (%s) — it only works on a machine that has that file", name, caPath)
	}
	if truthy(b["insecure-skip-tls-verify"]) {
		pf.add(Warning, "cluster %q skips TLS verification (insecure-skip-tls-verify) — the server's identity is not checked", name)
	} else if ca == "" && caPath == "" {
		pf.add(Info, "cluster %q has no CA; the system trust store will be used to verify it", name)
	}
}

func validateUser(pf *parsedFile, name string, b map[string]any) {
	cert, key := str(b, "client-certificate-data"), str(b, "client-key-data")
	certPath, keyPath := str(b, "client-certificate"), str(b, "client-key")
	hasCredential := false

	switch {
	case cert != "" || key != "":
		hasCredential = true
		switch {
		case cert == "":
			pf.add(Error, "user %q has client-key-data but no client-certificate-data", name)
		case key == "":
			pf.add(Error, "user %q has client-certificate-data but no client-key-data", name)
		default:
			certPEM, err1 := decode(cert)
			keyPEM, err2 := decode(key)
			if err1 != nil {
				pf.add(Error, "user %q: client-certificate-data is not valid base64", name)
				break
			}
			if err2 != nil {
				pf.add(Error, "user %q: client-key-data is not valid base64", name)
				break
			}
			certs, err := parseCertsPEM(certPEM)
			if err != nil {
				pf.add(Error, "user %q: client-certificate-data is invalid: %v", name, err)
				break
			}
			if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
				pf.add(Error, "user %q: the client certificate and key do not match or are unreadable (%v)", name, err)
			}
			leaf := certs[0]
			switch {
			case now().After(leaf.NotAfter):
				pf.add(Error, "user %q: the client certificate expired on %s — kubectl will be rejected", name, leaf.NotAfter.Format("2006-01-02"))
			case leaf.NotAfter.Sub(now()) < certExpirySoon:
				pf.add(Warning, "user %q: the client certificate expires on %s (within 30 days)", name, leaf.NotAfter.Format("2006-01-02"))
			case now().Before(leaf.NotBefore):
				pf.add(Warning, "user %q: the client certificate is not valid until %s", name, leaf.NotBefore.Format("2006-01-02"))
			}
		}
	case certPath != "" || keyPath != "":
		hasCredential = true
		pf.add(Warning, "user %q uses client certificate/key file paths — they only work on a machine that has those files", name)
	}

	if str(b, "token") != "" {
		hasCredential = true
	}
	if p := str(b, "tokenFile"); p != "" {
		hasCredential = true
		pf.add(Warning, "user %q reads its token from a file path (%s) — it only works on a machine that has that file", name, p)
	}
	if str(b, "username") != "" {
		hasCredential = true
		if str(b, "password") == "" {
			pf.add(Warning, "user %q has a username but no password", name)
		}
	}
	if ex, ok := b["exec"].(map[string]any); ok {
		hasCredential = true
		if cmd := str(ex, "command"); cmd == "" {
			pf.add(Error, "user %q has an exec credential plugin with no command", name)
		} else {
			pf.add(Warning, "user %q runs the command %q to obtain credentials — anyone using the merged file will execute it, so only merge files you trust", name, cmd)
		}
	}
	if ap, ok := b["auth-provider"].(map[string]any); ok {
		hasCredential = true
		pf.add(Warning, "user %q uses the auth-provider %q, which current kubectl versions no longer support", name, str(ap, "name"))
	}
	if !hasCredential {
		pf.add(Warning, "user %q has no credentials", name)
	}
}

// authDescription says how a user authenticates, without revealing secrets.
func authDescription(b map[string]any) string {
	switch {
	case str(b, "client-certificate-data") != "" || str(b, "client-certificate") != "":
		return "client certificate"
	case str(b, "token") != "" || str(b, "tokenFile") != "":
		return "token"
	case b["exec"] != nil:
		return "exec plugin"
	case b["auth-provider"] != nil:
		return "auth provider"
	case str(b, "username") != "":
		return "username/password"
	}
	return "none"
}

// ---- merging ---------------------------------------------------------------

type mergedConfig struct {
	clusters, users, contexts []named
	current                   string
}

func tagFor(file string, index int) string {
	base := filepath.Base(file)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(base, "-")
	base = strings.Trim(base, "-._")
	if base == "" || base == "config" || base == "kubeconfig" {
		return fmt.Sprintf("file%d", index+1)
	}
	return base
}

func mergeFiles(files []*parsedFile, opt Options, rep *Report) *mergedConfig {
	m := &mergedConfig{}
	used := map[string]map[string]bool{"cluster": {}, "user": {}, "context": {}}

	find := func(list []named, name string) *named {
		for i := range list {
			if list[i].name == name {
				return &list[i]
			}
		}
		return nil
	}
	// place adds entry e to list, resolving name clashes per the strategy, and
	// returns the name it ended up with.
	place := func(kind string, list *[]named, e named, tag string, pf *parsedFile) string {
		if existing := find(*list, e.name); existing != nil {
			if reflect.DeepEqual(existing.body, e.body) {
				rep.Files[pf.idx].Findings = append(rep.Files[pf.idx].Findings, Finding{Level: Info, File: pf.name,
					Msg: fmt.Sprintf("%s %q is identical to the one from %s and is merged into it", kind, e.name, existing.source)})
				return existing.name
			}
			if opt.OnConflict != OnConflictRename {
				rep.Global = append(rep.Global, Finding{Level: Error,
					Msg: fmt.Sprintf("%s %q is defined in both %s and %s with different settings. Pick “rename” to keep both, or fix one of the files.", kind, e.name, existing.source, pf.name)})
				// Still report the name as taken: callers resolve references
				// through it, and returning "" would add a misleading
				// "refers to a cluster that is not defined" on top of this.
				return existing.name
			}
			newName := e.name + "-" + tag
			for n := 2; used[kind][newName]; n++ {
				newName = fmt.Sprintf("%s-%s-%d", e.name, tag, n)
			}
			rep.Files[pf.idx].Findings = append(rep.Files[pf.idx].Findings, Finding{Level: Warning, File: pf.name,
				Msg: fmt.Sprintf("%s %q clashes with the one from %s, so it was renamed to %q", kind, e.name, existing.source, newName)})
			e.name = newName
		}
		used[kind][e.name] = true
		*list = append(*list, e)
		return e.name
	}

	for i, pf := range files {
		tag := tagFor(pf.name, i)
		clusterMap, userMap, contextMap := map[string]string{}, map[string]string{}, map[string]string{}
		for _, c := range pf.clusters {
			if n := place("cluster", &m.clusters, c, tag, pf); n != "" {
				clusterMap[c.name] = n
			}
		}
		for _, u := range pf.users {
			if n := place("user", &m.users, u, tag, pf); n != "" {
				userMap[u.name] = n
			}
		}
		for _, c := range pf.contexts {
			body := cloneMap(c.body)
			// Contexts must follow their cluster/user to the new names.
			if v, ok := clusterMap[str(body, "cluster")]; ok {
				body["cluster"] = v
			}
			if v, ok := userMap[str(body, "user")]; ok {
				body["user"] = v
			}
			e := named{name: c.name, body: body, source: pf.name}
			if n := place("context", &m.contexts, e, tag, pf); n != "" {
				contextMap[c.name] = n
			}
		}
		if m.current == "" && pf.current != "" {
			if n, ok := contextMap[pf.current]; ok {
				m.current = n
			}
		}
	}
	return m
}

func finish(rep *Report, m *mergedConfig, opt Options) {
	clusters := map[string]named{}
	for _, c := range m.clusters {
		clusters[c.name] = c
	}
	users := map[string]named{}
	for _, u := range m.users {
		users[u.name] = u
	}

	usedClusters, usedUsers := map[string]bool{}, map[string]bool{}
	for _, c := range m.contexts {
		cl, us := str(c.body, "cluster"), str(c.body, "user")
		cluster, okC := clusters[cl]
		user, okU := users[us]
		if cl != "" && !okC {
			rep.Global = append(rep.Global, Finding{Level: Error, File: c.source,
				Msg: fmt.Sprintf("context %q refers to cluster %q, which is not defined in any uploaded file", c.name, cl)})
		}
		if us != "" && !okU {
			rep.Global = append(rep.Global, Finding{Level: Error, File: c.source,
				Msg: fmt.Sprintf("context %q refers to user %q, which is not defined in any uploaded file", c.name, us)})
		}
		usedClusters[cl], usedUsers[us] = true, true
		rep.Contexts = append(rep.Contexts, ContextRow{
			Name: c.name, Cluster: cl, Server: str(cluster.body, "server"), User: us,
			Auth: authDescription(user.body), Source: c.source,
		})
		rep.ContextNames = append(rep.ContextNames, c.name)
	}
	for _, c := range m.clusters {
		if !usedClusters[c.name] {
			rep.Global = append(rep.Global, Finding{Level: Info, File: c.source, Msg: fmt.Sprintf("cluster %q is not used by any context", c.name)})
		}
	}
	for _, u := range m.users {
		if !usedUsers[u.name] {
			rep.Global = append(rep.Global, Finding{Level: Info, File: u.source, Msg: fmt.Sprintf("user %q is not used by any context", u.name)})
		}
	}

	if len(m.contexts) == 0 {
		rep.Global = append(rep.Global, Finding{Level: Error, Msg: "the merged result has no contexts — a kubeconfig needs at least one context to be usable"})
	}

	rep.CurrentContext = m.current
	if opt.CurrentContext != "" {
		found := false
		for _, n := range rep.ContextNames {
			if n == opt.CurrentContext {
				found = true
			}
		}
		if !found {
			rep.Global = append(rep.Global, Finding{Level: Error, Msg: fmt.Sprintf("the chosen current context %q does not exist in the merged result", opt.CurrentContext)})
		} else {
			rep.CurrentContext = opt.CurrentContext
		}
	}
	if rep.CurrentContext == "" && len(rep.ContextNames) > 0 {
		rep.Global = append(rep.Global, Finding{Level: Info, Msg: "no current context is set; pick one below, or switch with `kubectl config use-context`"})
	}
	rep.count()
}

// count tallies findings and decides whether a file can be generated.
func (r *Report) count() {
	r.Errors, r.Warnings = 0, 0
	tally := func(fs []Finding) {
		for _, f := range fs {
			switch f.Level {
			case Error:
				r.Errors++
			case Warning:
				r.Warnings++
			}
		}
	}
	tally(r.Global)
	for i := range r.Files {
		tally(r.Files[i].Findings)
		r.Files[i].Status = "ok"
		for _, f := range r.Files[i].Findings {
			if f.Level == Error {
				r.Files[i].Status = "error"
				break
			}
			if f.Level == Warning {
				r.Files[i].Status = "warning"
			}
		}
	}
	r.CanDownload = r.Errors == 0 && len(r.ContextNames) > 0
}

// ---- output ----------------------------------------------------------------

type outCluster struct {
	Name    string         `yaml:"name"`
	Cluster map[string]any `yaml:"cluster"`
}
type outContext struct {
	Name    string         `yaml:"name"`
	Context map[string]any `yaml:"context"`
}
type outUser struct {
	Name string         `yaml:"name"`
	User map[string]any `yaml:"user"`
}
type outConfig struct {
	APIVersion     string         `yaml:"apiVersion"`
	Clusters       []outCluster   `yaml:"clusters"`
	Contexts       []outContext   `yaml:"contexts"`
	CurrentContext string         `yaml:"current-context"`
	Kind           string         `yaml:"kind"`
	Preferences    map[string]any `yaml:"preferences"`
	Users          []outUser      `yaml:"users"`
}

func (m *mergedConfig) marshal(current string) []byte {
	out := outConfig{APIVersion: "v1", Kind: "Config", Preferences: map[string]any{}, CurrentContext: current}
	for _, c := range m.clusters {
		out.Clusters = append(out.Clusters, outCluster{Name: c.name, Cluster: c.body})
	}
	for _, c := range m.contexts {
		out.Contexts = append(out.Contexts, outContext{Name: c.name, Context: c.body})
	}
	for _, u := range m.users {
		out.Users = append(out.Users, outUser{Name: u.name, User: u.body})
	}
	if out.Clusters == nil {
		out.Clusters = []outCluster{}
	}
	if out.Users == nil {
		out.Users = []outUser{}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(out)
	_ = enc.Close()
	return buf.Bytes()
}

// ---- helpers ---------------------------------------------------------------

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func str(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

func decode(s string) ([]byte, error) {
	s = strings.Join(strings.Fields(s), "")
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

func parseCerts(b64 string) ([]*x509.Certificate, error) {
	raw, err := decode(b64)
	if err != nil {
		return nil, fmt.Errorf("not valid base64")
	}
	return parseCertsPEM(raw)
}

func parseCertsPEM(raw []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("a certificate could not be parsed")
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no PEM certificate found")
	}
	return certs, nil
}

// SortedContextNames returns the merged context names in a stable order for
// UI use.
func (r Report) SortedContextNames() []string {
	out := append([]string(nil), r.ContextNames...)
	sort.Strings(out)
	return out
}
