# Secrets

vigil can read the secrets it needs from an **OpenBao** or **Vault** you
already run (both speak the same API). So far that is the AI provider's key
for [explanations](home.md#ai-explanations); the tokens of alert receivers
are to follow.

vigil logs in with an AppRole (or a token) of its own and reads one KV
mount. Everything is set up in the interface, under **Secrets** (admins).

## What to create in OpenBao

A KV v2 mount for vigil, a policy that may read it, and an AppRole with
that policy. As an admin of your OpenBao (`bao`; with Vault the same
commands start with `vault`):

```bash
bao secrets enable -path=vigil kv-v2
```

```bash
bao policy write vigil - <<'EOF'
path "vigil/data/*"     { capabilities = ["read"] }
path "vigil/metadata/*" { capabilities = ["read", "list"] }
EOF
```

```bash
bao auth enable approle
```

```bash
bao write auth/approle/role/vigil token_policies=vigil token_ttl=1h token_max_ttl=24h
```

```bash
bao read auth/approle/role/vigil/role-id
```

```bash
bao write -f auth/approle/role/vigil/secret-id
```

The last two give the `role_id` and the `secret_id` that vigil's
**Secrets** page asks for. Then put the AI key in (it is read from standard
input, so it stays out of your shell history):

```bash
bao kv put vigil/anthropic api_key=-
```

Notes:

- **Leave out `secret_id_ttl` and `secret_id_num_uses`** (or set them to
  0), or vigil cannot log in again once the secret_id has expired or been
  used up, for example after a restart. vigil renews its token by itself
  and logs in again when the token reaches its maximum age.
- The `list` capability is optional: with it, the page offers the secrets
  under the mount to pick from; without it, you type the path.
- With `create` and `update` on `vigil/data/*` the policy would also let
  vigil write. vigil does not write yet; reading is all it needs.
- With a token instead of an AppRole, give vigil a renewable one (a
  periodic token is best). A token that cannot be renewed raises the alert
  `SecretStoreLoginExpiring` three days before it expires.

## Setting it up in vigil

1. **Secrets → Set up a secret store…**: the address (as vigil's server
   reaches it, e.g. `https://bao.example.com:8200`), the KV mount (`vigil`),
   AppRole with its `role_id` and `secret_id` (or a token), and the
   store's CA certificate if it is not signed by an authority the server
   trusts. **Test** logs in and shows the KV version, the policies, how
   long the login lives, and whether vigil may list and read.
2. **Test and save** asks for your password again (or the admin token, if
   you signed in with it).
3. In the table below, **Choose…** next to *AI provider key*: the secret's
   path under the mount (`anthropic`) and the field in it (`api_key`).
   **Test** reads it again and asks the provider whether it accepts the key
   (listing its models, which costs nothing).

For the AI key to come from the store, the server config's `ai` section
must not have `api_key_file`; the rest of that section (provider, model,
budget) stays in the config file:

```yaml
ai:
  provider: anthropic
  monthly_budget: 20
  data: [logs]
```

## How vigil keeps it

- The `secret_id` (or token) is kept in vigil's database **encrypted**
  (AES-256-GCM) with a key in `secrets.key` next to it in `data_dir` (mode
  0600). A copy of the database alone reveals nothing; back up and protect
  the key file accordingly.
- The saved `secret_id` is only reused for the **same address and login**.
  Change the address, the role or the CA, and it has to be entered again:
  otherwise someone with an admin session could point vigil at a server of
  their own and collect its login.
- Values are kept **in memory only** and read again every five minutes. No
  API answers with a value; the page shows whether it is there, its version
  and its last four characters.
- **When the store does not answer** (sealed, down, login refused) vigil
  goes on with the values it read last and raises `SecretStoreDown` after
  five minutes. A value it never read (for example right after a restart
  while the store is sealed) is missing until the store answers again; for
  the AI key that means Explain fails and everything else works.
- `vigil_secret_store_up` (1 or 0) and
  `vigil_secret_store_token_expiry_seconds` (how long vigil's login has
  left) are in the metric store.

## Not yet

- Setting a value from vigil, and other stores than OpenBao and Vault.
- The receivers' tokens (Telegram, mail, Slack) from the store. They stay
  in files: alerts must go out even when a store is sealed, and a value
  read before a restart is not kept across it.
