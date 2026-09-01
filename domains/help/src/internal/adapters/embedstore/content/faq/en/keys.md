## What this tab is for
API Keys issues and revokes the gateway keys. Every key resolves **org + team + app**: that is what separates your cost by team and by app. We store only the hash — the value appears once, at creation.

## How to use it
Pick an existing team and app (created under Teams & Apps) and issue. Copy the key right away — we do not show it again. Point your app's `base_url` at the gateway and use the key as a Bearer token.

## Common questions
- **I cannot create a team or app here.** By design: creation lives in Teams & Apps; here you only associate a key with something that already exists.
- **Is a key "for a team" or "for an app"?** The key always carries the team; the app can be a specific one or `default` (the whole team).
- **I lost the key.** It cannot be recovered (we only keep the hash). Revoke it and issue another.
