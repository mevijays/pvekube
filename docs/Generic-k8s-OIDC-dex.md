---
layout: default
title: OIDC Authentication with Dex
permalink: /oidc-dex/
---

# Generic Kubernetes OIDC Authentication with Dex + LDAP

A complete, tested runbook for wiring any Kubernetes cluster's API server up
to OIDC authentication via [Dex](https://dexidp.io/) as a broker in front of
an LDAP directory. Every gotcha in the "Troubleshooting" sections below is a
real failure hit and fixed while building this out — not hypothetical.

**Scope**: authentication only. RBAC (who can do what once authenticated) is
covered at the end and is mandatory — an OIDC-authenticated user with no
RBAC binding can log in successfully and still get `Forbidden` on everything.

## Architecture

```
LDAP user ──(browser login)──> Dex (broker) ──(LDAP bind)──> LDAP server
                │
                ▼
     ID token (JWT: email, groups claims)
                │
                ▼
kubectl (via kubelogin plugin) ──Bearer token──> kube-apiserver
                                                       │
                                          validates token against
                                          Dex's public keys (JWKS),
                                          extracts email/groups,
                                          RBAC decides what's allowed
```

kube-apiserver **never** talks to Dex during login, and Dex **never** talks
to Kubernetes. kube-apiserver only fetches Dex's public signing keys (via
the issuer URL) to verify tokens it's handed — a much smaller trust
relationship than it might look like.

---

## 1. Self-signed CA setup

On the Dex host (or wherever you keep your homelab CA):

```bash
mkdir -p ~/dex-pki && cd ~/dex-pki

# CA private key + self-signed root cert (10 year validity)
openssl genrsa -out ca.key 4096
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 \
  -subj "/CN=internal-ca/O=Home Lab" \
  -out ca.crt
```

Keep `ca.key` private and offline if you can — it can sign anything trusted
by everything that trusts this CA. `ca.crt` is what gets distributed to
kube-apiserver and to every operator's laptop.

## 2. Generate and sign Dex's server certificate

Modern TLS clients (Go's crypto stack included, which is what
kube-apiserver and `kubectl` use) reject certificates that only have a CN
and no `subjectAltName` — the SAN is mandatory, not optional:

```bash
cd ~/dex-pki

cat > dex.ext <<'EOF'
subjectAltName = DNS:auth.mylab.lan
EOF

openssl genrsa -out dex.key 2048
openssl req -new -key dex.key -subj "/CN=auth.mylab.lan" -out dex.csr
openssl x509 -req -in dex.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out dex.crt -days 825 -sha256 -extfile dex.ext

# sanity check — must show the SAN, or every client that checks it will reject the cert
openssl x509 -in dex.crt -noout -text | grep -A1 "Subject Alternative Name"
```

Make sure `auth.mylab.lan` actually resolves to the Dex host — internal DNS
entry, or a manual `/etc/hosts` line on every machine that needs to reach it
(the Dex host itself, every Kubernetes control-plane node, and every
operator's laptop).

## 3. Run Dex via docker-compose

`~/dex/docker-compose.yml`:

```yaml
services:
  dex:
    image: dexidp/dex:v2.41.1
    container_name: dex
    restart: unless-stopped
    ports:
      - "443:5556"
    volumes:
      - ./config.yaml:/etc/dex/config.yaml:ro
      - ~/dex-pki/dex.crt:/etc/dex/tls.crt:ro
      - ~/dex-pki/dex.key:/etc/dex/tls.key:ro
      - ./ldap-ca.crt:/etc/dex/ldap-ca.crt:ro   # the CA that signed your LDAP server's cert, if it uses one
      - dex-data:/var/dex
    command: ["dex", "serve", "/etc/dex/config.yaml"]

volumes:
  dex-data:
```

`~/dex/config.yaml`:

```yaml
# REQUIRED. This is Dex's actual OIDC issuer URL — it gets embedded in every
# token's "iss" claim and MUST match --oidc-issuer-url on kube-apiserver
# exactly (scheme, host, no trailing slash). Do not confuse this with the
# unrelated frontend.issuer further down (that's just login-page branding
# text) — a config missing THIS field but having frontend.issuer set LOOKS
# complete at a glance and is not. See Troubleshooting #1.
issuer: https://auth.mylab.lan

storage:
  type: sqlite3
  config:
    file: /var/dex/dex.db

web:
  https: 0.0.0.0:5556
  tlsCert: /etc/dex/tls.crt
  tlsKey: /etc/dex/tls.key

connectors:
- type: ldap
  id: ldap
  name: LDAP
  config:
    host: 172.16.1.10:636
    insecureNoSSL: false
    rootCA: /etc/dex/ldap-ca.crt
    userSearch:
      baseDN: ou=people,dc=mylab,dc=lan
      filter: "(objectClass=inetOrgPerson)"
      username: uid
      idAttr: uid
      emailAttr: mail
      nameAttr: cn
    groupSearch:
      baseDN: ou=groups,dc=mylab,dc=lan
      filter: "(objectClass=posixGroup)"
      userMatchers:
      - userAttr: uid
        groupAttr: memberUid
      nameAttr: cn

oauth2:
  skipApprovalScreen: true

# REQUIRED. Without this block Dex has no registered client named
# "kubernetes" at all, and kubelogin's token request fails with
# "Bad Request: Invalid client_id provided." See Troubleshooting #2.
staticClients:
- id: kubernetes
  name: Kubernetes
  secret: CHANGE_ME   # openssl rand -hex 32 — used by kubelogin to authenticate to Dex, never seen by kube-apiserver
  redirectURIs:
  - http://localhost:8000   # kubelogin's own default local callback port — see section 6

frontend:
  dir: /web
  theme: dex
  issuer: "MyLab SSO"   # COSMETIC ONLY — display name on the login page. Not the OIDC issuer. See the top-level issuer: field above.
```

Generate a real secret before starting:
```bash
openssl rand -hex 32
# paste the result into staticClients[0].secret above
```

## 4. Start Dex and verify

```bash
cd ~/dex
docker compose up -d
docker compose logs -f dex
```

Watch the startup log for the static client and LDAP connector loading
without errors. Then confirm the discovery endpoint actually serves:

```bash
curl --cacert ~/dex-pki/ca.crt https://auth.mylab.lan/.well-known/openid-configuration
```

You should get back a JSON document with `"issuer": "https://auth.mylab.lan"`
matching exactly what you set. If the `issuer` field in that response
doesn't match, fix `config.yaml` and restart — **Dex does not hot-reload its
config file on change**, you must restart the container every time you edit
it (`docker compose restart dex`).

---

## 5. Kubernetes API server configuration

`kube-apiserver` is a **static pod** under kubeadm — kubelet watches
`/etc/kubernetes/manifests/` and automatically recreates the pod whenever a
file there changes. No `kubectl apply`, no manual restart command needed.
This also means editing it wrong can take your only API server down with no
`kubectl` access to fix it — read the whole troubleshooting section before
you start, and on a single-control-plane cluster keep a way to reach the
node directly (SSH, or your hypervisor's console) as a safety net.

**On every control-plane node:**

**1. Place Dex's CA:**
```bash
sudo mkdir -p /etc/kubernetes/pki
sudo cp ca.crt /etc/kubernetes/pki/oidc-ca.pem
sudo chmod 0644 /etc/kubernetes/pki/oidc-ca.pem
```

**2. Before touching the manifest, prove the node can actually reach Dex** —
this rules out DNS/network/CA problems before you're debugging a
crash-looping API server:
```bash
curl --cacert /etc/kubernetes/pki/oidc-ca.pem https://auth.mylab.lan/.well-known/openid-configuration
```
If this fails here, fix it here first. Editing the apiserver manifest won't
help a network problem.

**3. Edit `/etc/kubernetes/manifests/kube-apiserver.yaml`** — add these to
the existing `spec.containers[0].command` list, keeping the same
indentation as every other `- --flag=value` entry around it:

```yaml
    - --oidc-issuer-url=https://auth.mylab.lan
    - --oidc-client-id=kubernetes
    - --oidc-username-claim=email
    - --oidc-groups-claim=groups
    - --oidc-ca-file=/etc/kubernetes/pki/oidc-ca.pem
    - "--oidc-username-prefix=oidc:"
    - "--oidc-groups-prefix=oidc:"
```

Note the **quotes** around the last two — see Troubleshooting #4, this is
not optional styling.

Save the file. Wait ~20–30 seconds for kubelet to pick it up.

**4. Verify:**
```bash
kubectl get pod -n kube-system -l component=kube-apiserver -o wide
```
The `AGE` column resetting to a few seconds and restart count staying at
`0` (not climbing) means it applied and the process is staying up cleanly.
If `AGE` doesn't change at all after a minute or more, kubelet never
accepted the file — go to Troubleshooting #4.

**5. Repeat on every other control-plane node** if running HA (odd number
of replicas) — each one is an independent static pod, none of this
propagates automatically.

### Why `oidc-username-prefix`/`oidc-groups-prefix` matter

Per the [Kubernetes docs](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#openid-connect-tokens):
if `--oidc-username-claim` is anything other than `email` and you don't set
`--oidc-username-prefix`, Kubernetes **silently** prepends `<issuer-url>#`
to the authenticated username. Setting an explicit prefix (`oidc:` here)
avoids that surprise and makes RBAC subject names predictable — the actual
identity Kubernetes sees becomes `oidc:<email>`, and groups become
`oidc:<ldap-group-cn>`. Both are used exactly this way in the RBAC section
below.

---

## Troubleshooting

**1. `curl .../.well-known/openid-configuration` returns an issuer that
doesn't match, or Dex won't start at all**
Check `issuer:` is set at the **top level** of `config.yaml`, not confused
with the unrelated `frontend.issuer` (cosmetic display text only). A config
with `frontend.issuer` set but no top-level `issuer:` looks complete at a
glance and isn't.

**2. `kubectl oidc-login get-token` fails with `Bad Request: Invalid
client_id provided.`**
Dex has no registered `staticClients` entry matching your `--oidc-client-id`
— either the block is missing from `config.yaml`, or you edited the file
but never restarted Dex (`docker compose restart dex` — config changes are
not hot-reloaded).

**3. You get a token back fine, but `kube-apiserver` rejects it / `kubectl`
gets `Unauthorized`**
Decode the token and check its claims — a missing `email` claim is the most
common cause, and it happens when the OIDC client didn't request the
`email` scope (`groups` and `email` are both opt-in scopes, requesting one
doesn't imply the other):
```bash
TOKEN="eyJhbGci..."   # the "token" field from oidc-login get-token's output
python3 -c "import sys,base64,json; p=sys.argv[1].split('.')[1]; p+='='*(-len(p)%4); print(json.dumps(json.loads(base64.urlsafe_b64decode(p)),indent=2))" "$TOKEN"
```
Fix: add `--oidc-extra-scope=email` alongside `--oidc-extra-scope=groups`
wherever kubelogin is invoked (see section 6).

**4. You edited the manifest, waited, and the pod's `AGE`/restart count
never changes — nothing happens, no error visible anywhere obvious**
kubelet **silently ignores** a static pod manifest it can't parse — it
doesn't crash anything, it just keeps running whatever was there before
forever. Check kubelet's own log, not the pod's:
```bash
journalctl -u kubelet --since "10 minutes ago" | grep -i "could not process manifest"
```
A real example of this exact failure:
```
"Could not process manifest file" err="/etc/kubernetes/manifests/kube-apiserver.yaml:
couldn't parse as pod(json: cannot unmarshal object into Go struct field
Container.spec.containers.command of type string), please check config file"
```
Root cause: an unquoted YAML list entry ending in a bare colon —
`- --oidc-username-prefix=oidc:` — gets parsed as a mapping key with a null
value (an object), not a plain string, because YAML treats a trailing
`: ` (or `:` at end-of-line) as a key/value indicator unless the whole
scalar is quoted. **Any flag value that itself contains a colon must be
quoted**: `- "--oidc-username-prefix=oidc:"`. This is the single most
time-consuming failure mode to diagnose blind — it produces no crash, no
restart, no obviously-related symptom, just a stale pod that quietly never
updates.

**5. The pod actually crash-loops (visible in `crictl ps -a` as repeated
`Exited`, restart count climbing)**
This is a different failure class from #4 — the YAML parsed fine, but
`kube-apiserver` itself is failing at startup. Almost always the CA file:
```bash
crictl ps -a | grep apiserver
crictl logs <container-id>   # get the ID from the line above
ls -la /etc/kubernetes/pki/oidc-ca.pem   # confirm it actually exists at the path the flag references
```
`--oidc-ca-file` is read once at process startup — if the path is wrong or
the file is missing, `kube-apiserver` exits immediately. On a single
control-plane cluster this means **total `kubectl` access loss** until
fixed. Recovery: SSH/console into the node directly (this is a local file
edit, not an API-mediated one — it works even with `kubectl` completely
dead) and fix or remove the offending flag, then wait for kubelet to
recreate the pod.

**6. Authenticated fine, but every command returns `Forbidden`**
Expected and correct — OIDC only authenticates, it grants zero permissions
on its own. See the RBAC section below.

**7. General validation checklist after any change**
```bash
kubectl get pod -n kube-system -l component=kube-apiserver -o wide   # AGE reset + restart count = 0 → applied cleanly
crictl ps -a | grep apiserver                                        # crash history, if any
journalctl -u kubelet --since "5 minutes ago" | grep -i "manifest\|apiserver"
kubectl get nodes                                                    # basic end-to-end reachability
```

---

## 6. Install kubelogin (`kubectl oidc-login`)

Pick one:

```bash
# Homebrew (macOS/Linux)
brew install int128/kubelogin/kubelogin

# krew (kubectl's own plugin manager)
kubectl krew install oidc-login

# Direct binary (no package manager)
curl -L https://github.com/int128/kubelogin/releases/latest/download/kubelogin_linux_amd64.zip -o kubelogin.zip
unzip kubelogin.zip
sudo mv kubelogin /usr/local/bin/kubectl-oidc_login
chmod +x /usr/local/bin/kubectl-oidc_login
```

Test the login flow in isolation before touching any kubeconfig — this
proves Dex + LDAP + kubelogin work together without also debugging kubectl
config at the same time:

```bash
kubectl oidc-login get-token \
  --oidc-issuer-url=https://auth.mylab.lan \
  --oidc-client-id=kubernetes \
  --oidc-client-secret=<the staticClients secret> \
  --oidc-extra-scope=groups \
  --oidc-extra-scope=email \
  --certificate-authority=/path/to/dex-ca.crt
```

Your browser opens to Dex's LDAP login form. On success this prints a JSON
blob with a `token` field — a JWT. That's success for this step.

## 7. Set up kubeconfig

Build on an existing working kubeconfig (e.g. one with client-cert auth) —
its `clusters:` entry (server address + cluster CA) is already correct, you
only need to add a new user/context:

```bash
export KUBECONFIG=~/path/to/existing-kubeconfig.yaml

kubectl config set-credentials oidc \
  --exec-api-version=client.authentication.k8s.io/v1 \
  --exec-command=kubectl \
  --exec-arg=oidc-login \
  --exec-arg=get-token \
  --exec-arg=--oidc-issuer-url=https://auth.mylab.lan \
  --exec-arg=--oidc-client-id=kubernetes \
  --exec-arg=--oidc-client-secret=<the staticClients secret> \
  --exec-arg=--oidc-extra-scope=groups \
  --exec-arg=--oidc-extra-scope=email \
  --exec-arg=--certificate-authority=/path/to/dex-ca.crt

kubectl config set-context CLUSTER_NAME-oidc --cluster=CLUSTER_NAME --user=oidc
kubectl config use-context CLUSTER_NAME-oidc
```

Or hand-write a standalone one:

```yaml
apiVersion: v1
kind: Config
clusters:
- name: my-cluster
  cluster:
    server: https://<control-plane-endpoint>:6443
    certificate-authority-data: <base64 of the cluster's /etc/kubernetes/pki/ca.crt>
contexts:
- name: my-cluster-oidc
  context:
    cluster: my-cluster
    user: oidc
current-context: my-cluster-oidc
users:
- name: oidc
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kubectl
      args:
      - oidc-login
      - get-token
      - --oidc-issuer-url=https://auth.mylab.lan
      - --oidc-client-id=kubernetes
      - --oidc-client-secret=<the staticClients secret>
      - --oidc-extra-scope=groups
      - --oidc-extra-scope=email
      - --certificate-authority=/path/to/dex-ca.crt
      interactiveMode: IfAvailable
```

Note: `certificate-authority-data` in `clusters:` is the **cluster's own**
API-server CA — a different cert from the Dex CA passed via
`--certificate-authority` in the exec args. Don't mix them up.

Test:
```bash
kubectl get nodes
```
Browser opens, log in with your LDAP credentials, and you should get either
a resource list (if RBAC is already set up) or `Forbidden` (if not — see
below). Either is a sign auth itself is working; only `Unauthorized`
indicates an auth problem, not `Forbidden`.

---

## 8. RBAC — granting permissions to LDAP groups

Nothing above grants any permissions. Two groups, two different levels of
access, both driven off LDAP group membership via the `groups` claim Dex
already resolved.

**Group name format matters**: with `--oidc-groups-prefix=oidc:` set on the
API server, the group name Kubernetes actually sees is `oidc:<ldap-group-cn>`
— not the bare LDAP name. Both `RoleBinding`s below use that prefixed form.
Both LDAP groups (`k8susers`, `devops`) need to already exist under your
`groupSearch.baseDN` (`ou=groups,dc=mylab,dc=lan`) as `posixGroup` entries
with the right users listed in `memberUid` — Dex surfaces whatever LDAP
groups match automatically, no Dex-side config change needed to add a group.

### Read-only cluster access for the `k8susers` LDAP group

Kubernetes ships a built-in `view` `ClusterRole` — read access to almost
everything cluster-wide, deliberately excluding `Secrets` (which may hold
sensitive data) and RBAC objects themselves. This is the standard
"cluster-reader" role; bind it cluster-wide to the group:

```bash
kubectl create clusterrolebinding oidc-k8susers-view \
  --clusterrole=view \
  --group="oidc:k8susers"
```

Verify:
```bash
kubectl get clusterrolebinding oidc-k8susers-view -o yaml
```

### Full admin access for the `devops` LDAP group

Kubernetes ships a built-in `cluster-admin` `ClusterRole` — unrestricted
access to everything, every namespace, every verb. Bind it cluster-wide to
the group:

```bash
kubectl create clusterrolebinding oidc-devops-admin \
  --clusterrole=cluster-admin \
  --group="oidc:devops"
```

Verify:
```bash
kubectl get clusterrolebinding oidc-devops-admin -o yaml
```

### Confirming it actually works, as a specific user

```bash
# as an admin, check what a given identity can do without needing them to log in
kubectl auth can-i get pods --all-namespaces --as="oidc:someone@mylab.lan" --as-group="oidc:k8susers"
kubectl auth can-i delete deployments -n default --as="oidc:someone@mylab.lan" --as-group="oidc:devops"
```

`--as-group` has to be passed explicitly for group-based checks —
`--as` alone impersonates only the user identity, not their groups.
