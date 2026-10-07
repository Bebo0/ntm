package policy

// The scripts `ntm safety install` (and the serve install endpoint) put in
// front of git, rm and Claude Code's Bash tool. The wrappers ask `ntm safety
// check --hook` and the Claude hook runs `ntm safety claude-hook`; each refuses
// on a non-zero exit. In hook mode the check itself files
// the refusal (blocked log with the tmux session, and the session's
// blocked-command metric), so the scripts do no logging of their own
// (bd-cl6me). One copy serves both installers.

// GitWrapperScript intercepts destructive git commands.
const GitWrapperScript = `#!/bin/bash
# NTM Safety Wrapper for git
# Intercepts destructive git commands

REAL_GIT=$(which -a git | grep -v "$HOME/.ntm/bin" | head -1)
if [ -z "$REAL_GIT" ]; then
    REAL_GIT="/usr/bin/git"
fi

# Check command against policy (include "git" in the command string). In hook
# mode the check records a refusal itself.
check_result=$(ntm safety check "git $*" --json --hook 2>&1)
exit_code=$?

# ntm safety check exits 0 for allow, 1 for block/approve
if [ $exit_code -ne 0 ]; then
    action=$(echo "$check_result" | jq -r '.action // "block"' 2>/dev/null)
    reason=$(echo "$check_result" | jq -r '.reason // "Policy violation"' 2>/dev/null)

    if [ "$action" = "approve" ]; then
        echo "NTM Safety: Command requires approval" >&2
        echo "  Reason: $reason" >&2
        echo "  Command: git $*" >&2
        echo "  This wrapper is advisory: it refused the command but queued no approval request." >&2
        echo "  Approval-gated ntm commands (e.g. 'ntm locks force-release') request approval when run; decide with 'ntm approve'." >&2
    else
        echo "NTM Safety: Command blocked" >&2
        echo "  Reason: $reason" >&2
        echo "  Command: git $*" >&2
    fi
    exit 1
fi

# Pass through to real git
exec "$REAL_GIT" "$@"
`

// RmWrapperScript intercepts destructive rm commands.
const RmWrapperScript = `#!/bin/bash
# NTM Safety Wrapper for rm
# Intercepts destructive rm commands

REAL_RM=$(which -a rm | grep -v "$HOME/.ntm/bin" | head -1)
if [ -z "$REAL_RM" ]; then
    REAL_RM="/bin/rm"
fi

# Check command against policy. In hook mode the check records a refusal itself.
check_result=$(ntm safety check "rm $*" --json --hook 2>&1)
exit_code=$?

# ntm safety check exits 0 for allow, 1 for block/approve
if [ $exit_code -ne 0 ]; then
    action=$(echo "$check_result" | jq -r '.action // "block"' 2>/dev/null)
    reason=$(echo "$check_result" | jq -r '.reason // "Policy violation"' 2>/dev/null)

    if [ "$action" = "approve" ]; then
        echo "NTM Safety: Command requires approval" >&2
        echo "  Reason: $reason" >&2
        echo "  Command: rm $*" >&2
        echo "  This wrapper is advisory: it refused the command but queued no approval request." >&2
        echo "  Approval-gated ntm commands (e.g. 'ntm locks force-release') request approval when run; decide with 'ntm approve'." >&2
    else
        echo "NTM Safety: Command blocked" >&2
        echo "  Reason: $reason" >&2
        echo "  Command: rm $*" >&2
    fi
    exit 1
fi

# Pass through to real rm
exec "$REAL_RM" "$@"
`

// ClaudeHookScript is the Claude Code PreToolUse hook `ntm safety install`
// writes and registers in ~/.claude/settings.json for the Bash tool. Claude
// Code pipes the event JSON on stdin; `ntm safety claude-hook` parses it in Go
// (no jq: the old script read the payload with jq and allowed every command
// when jq was missing), checks the command in hook mode, and exits 2 with the
// reason on stderr to refuse it. A hook that cannot find ntm refuses rather
// than waving commands through unchecked, and says how to remove itself.
const ClaudeHookScript = `#!/bin/sh
# NTM Safety Hook for Claude Code (PreToolUse, Bash tool).
# Registered in ~/.claude/settings.json by 'ntm safety install'.
if command -v ntm >/dev/null 2>&1; then
    exec ntm safety claude-hook
fi
echo "BLOCKED: the ntm safety hook could not find ntm on PATH, so this command was not checked." >&2
echo "Put ntm on PATH, or remove the ntm-safety.sh entry from hooks.PreToolUse in ~/.claude/settings.json ('ntm safety uninstall' does this)." >&2
exit 2
`
