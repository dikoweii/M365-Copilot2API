# Privacy-safe release checklist

The release branch is built as a new root snapshot. Existing tracked files and
Git history are not trusted merely because they are already committed.

## Before creating a snapshot

1. Rotate credentials that may have appeared in logs, screenshots, binaries,
   shell history, or previous Git objects.
2. Keep accounts, tokens, API keys, administrator passwords, proxy credentials,
   databases, logs, backups, screenshots, and deployment target files outside
   the repository.
3. Run `scripts/privacy-scan.ps1`. It reports only finding type, path, and line;
   it never prints the matched value.
4. Review every untracked and modified file. Do not use `git add .` in the
   development repository.

## Create the clean root snapshot

```powershell
./scripts/create-clean-snapshot.ps1 -OutputRoot ../M365-Copilot2API-clean
git -C ../M365-Copilot2API-clean status --short
git -C ../M365-Copilot2API-clean diff --cached --check
```

The output repository has no parent commits. Review its complete staged file
list and staged diff before committing. Push it to a new branch first; deleting
or rewriting older remote branches and tags is a separate destructive action.

## Required release checks

```powershell
go test -count=1 ./...
go vet ./...
go build ./...
./scripts/privacy-scan.ps1
git diff --cached --check
```

After pushing, clone only the clean branch into an empty directory and repeat
the privacy scan. Confirm that old screenshot and executable objects are not
reachable from that branch.
