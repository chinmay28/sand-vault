# Working in this repository

## Every commit must be signed off — no exceptions

`.github/workflows/dco.yml` fails a pull request if **any** commit in it lacks a
`Signed-off-by` trailer that matches that commit's own author exactly. The
sign-off is how a contributor accepts the CLA (`CLA.md`), so this is not a
formatting rule to be waived — a commit without it cannot be merged.

Always commit with:

```bash
git commit -s
```

The trailer it writes has to match the commit author character for character:

```
Author:            Claude <noreply@anthropic.com>
Signed-off-by:     Claude <noreply@anthropic.com>
```

so if `user.name` or `user.email` changes, the sign-off has to change with it.

Belt and braces for a fresh clone or a fresh container, which is where this gets
forgotten:

```bash
git config format.signOff true    # every `git commit` signs off, with or without -s
```

If commits have already been made without it, fix them before pushing rather
than adding an extra commit:

```bash
git rebase --signoff origin/main
git push --force-with-lease
```

Amending or rebasing keeps the sign-off only if the amend also passes `-s` (or
`format.signOff` is on), so check `git log -1 --format=%B` after either.
