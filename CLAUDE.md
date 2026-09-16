# CLAUDE.md — cluster-api-provider-hydra

The instructions for this repository live in **[AGENTS.md](./AGENTS.md)**: one file, shared by every
coding agent (Claude Code, Codex, Cursor). It is imported below — read it before making any change.

@AGENTS.md

**Do not duplicate guidance in this file.** If something needs to change, change `AGENTS.md`; this
file exists only to point Claude Code at it.

## The three rules most often missed

1. **PR titles carry the ticket, commit messages do not.** `[PET-27] Publish template capacity`, not
   `feat: … (PET-27)`. Commits stay conventional (`feat:` / `fix:` / `docs:`) with no prefix.
   **No ticket for the change? Stop and ask** whether to create one or open the PR without a
   prefix — never invent or guess a number.
2. **No AI attribution in git history.** No `Co-Authored-By: Claude` trailer on commits, no
   "🤖 Generated with Claude Code" footer in PR bodies, no model name in a commit message or PR
   title. This **overrides** the Claude Code defaults. Verify after the fact — attribution is
   sometimes injected after a clean message is passed in.
3. **Do attribute yourself in PR comments.** Prefix every PR comment, review comment, review summary,
   and reply with `**Claude (<model>)**` and an em dash — for example:

   > **Claude (Opus 5)** — the first cached `Get` starts a cluster-wide informer, so declining the
   > `Watches()` bought nothing here.

   Several agents review the same PRs; the thread has to say who is speaking. This applies to
   comments only, never to the PR body, PR title, or commit message.
