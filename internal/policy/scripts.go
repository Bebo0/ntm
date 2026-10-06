package policy

// The scripts `ntm safety install` (and the serve install endpoint) put in
// front of git, rm and Claude Code's Bash tool. Each asks `ntm safety check
// --hook` and refuses on a non-zero exit. In hook mode the check itself files
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

// ClaudeHookScript is a Claude Code PreToolUse hook that validates Bash
// commands.
const ClaudeHookScript = `#!/bin/bash
# NTM Safety Hook for Claude Code
# PreToolUse hook that validates Bash commands

# Claude Code command hooks receive the event payload as JSON on stdin.
HOOK_INPUT="$(cat)"
if [ -n "$HOOK_INPUT" ] && command -v jq >/dev/null 2>&1; then
    TOOL_NAME="$(printf '%s' "$HOOK_INPUT" | jq -r '.tool_name // empty' 2>/dev/null)"
    COMMAND="$(printf '%s' "$HOOK_INPUT" | jq -r '.tool_input.command // empty' 2>/dev/null)"
else
    TOOL_NAME=""
    COMMAND=""
fi

# Fall back to legacy env vars if a caller still provides them directly.
if [ -z "$TOOL_NAME" ]; then
    TOOL_NAME="${CLAUDE_TOOL_NAME:-}"
fi
if [ -z "$COMMAND" ]; then
    COMMAND="${CLAUDE_TOOL_INPUT_command:-}"
fi

# Only process Bash tool calls
if [ "$TOOL_NAME" != "Bash" ]; then
    exit 0
fi

if [ -z "$COMMAND" ]; then
    exit 0
fi

# Check against policy. In hook mode the check records a refusal itself.
check_result=$(ntm safety check "$COMMAND" --json --hook 2>&1)
exit_code=$?

# ntm safety check exits 0 for allow, 1 for block/approve
if [ $exit_code -ne 0 ]; then
    action=$(echo "$check_result" | jq -r '.action // "block"' 2>/dev/null)
    reason=$(echo "$check_result" | jq -r '.reason // "Policy violation"' 2>/dev/null)

    # Return error to Claude Code
    if [ "$action" = "approve" ]; then
        echo "APPROVAL REQUIRED: $reason" >&2
        echo "This check is advisory: the command was refused but no approval request was queued." >&2
        echo "Approval-gated ntm commands (e.g. 'ntm locks force-release') request approval when run; decide with 'ntm approve'." >&2
    else
        echo "BLOCKED: $reason" >&2
    fi
    exit 2
fi

exit 0
`
