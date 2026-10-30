# Sourced by inside.sh: `send` against the real herdr and a fake Claude,
# every outcome of `herdr agent prompt` mapped to its exit code.
need_fake
sender() { env "${P}AGENT=judge-sender" "${P}ROLE=thread" "${P}SCOPE=$SCOPE" "$T" "$@"; }

echo "the quick brown fox 0xC0FFEE" | sender send fake; rc=$?
check "send: exit 0 on agent_prompted" 0 "$rc"
has "send: header and body on the target's screen" "$(screen fake)" "[FROM: judge-sender]" "0xC0FFEE"
settled fake

printf 'from a file 0xF11E\n' > /home/agent/body.md
sender send fake --file /home/agent/body.md </dev/null >/dev/null; rc=$?
check "send: exit 0 with the body from --file" 0 "$rc"
has "send: file body on the target's screen" "$(screen fake)" "[FROM: judge-sender]" "0xF11E"
settled fake

echo "hello" | sender send nobody; rc=$?
check "send: exit 4 on agent_not_found" 4 "$rc"

echo "hello" | env "${P}AGENT=judge-sender" "$T" --scope no-such-scope send fake >/dev/null 2>&1; rc=$?
check "send: exit 5 when herdr itself fails" 5 "$rc"

# A slow rest of the message must not delay the first frame: with the gather
# window past herdr's 5 s stall limit, working must still show at once.
mkdir -p /home/agent/.fake-claude
echo 7 > /home/agent/.fake-claude/gather-secs
t0=$SECONDS
echo "slow gather" | sender send fake; rc=$?
check "send: exit 0 when the message is gathered slowly" 0 "$rc"
rm /home/agent/.fake-claude/gather-secs
# Let the 7 s gather and the 2 s turn end whatever the exit code was, so a
# failure here cannot swallow the next arm's input.
while [ $((SECONDS - t0)) -lt 11 ]; do sleep 0.5; done
screen fake >/dev/null
settled fake

touch /home/agent/.fake-claude/stall
echo "please STALL" | sender send fake; rc=$?
rm /home/agent/.fake-claude/stall
check "send: exit 2 on agent_prompt_stalled" 2 "$rc"

echo "please BLOCK" | sender send fake; rc=$?
check "send: exit 0 before the prompt appears" 0 "$rc"
"${S[@]}" agent wait fake --until blocked --timeout 20000 >/dev/null
out=$(echo "again" | sender send fake 2>&1); rc=$?
check "send: exit 3 on agent_blocked" 3 "$rc"
has "send: blocked target's screen is printed" "$out" "Do you want to proceed?"

# A Claude Code session paused on its `Session paused` dialog (refused,
# retried, gone on with a tool, the turn's next refusal not recorded), which
# herdr reads as idle, gets nothing typed: exit 8. One waiting at its prompt
# after a stop with no fallback gets the message.
check "send: paused-s starts idle" idle "$(start_fake paused-s)"
check "send: prompt-s starts idle" idle "$(start_fake prompt-s)"
session paused-s 00000000-0000-4000-8000-0000000000b1 "$(said s-1 msg-s1 claude-test-large end_turn)" \
  "$(said s-2 msg-s2 claude-test-large refusal)" \
  '{"type":"user","uuid":"s-3","message":{"content":[{"type":"tool_result","tool_use_id":"t-s2","content":"0xREFUSEDTEXT"}]}}' \
  "$(said s-4 msg-s4 claude-test-large tool_use)"
session prompt-s 00000000-0000-4000-8000-0000000000b2 "$(said s-1 msg-s1 claude-test-large end_turn)" \
  "$(said s-2 msg-s2 claude-test-large refusal)" \
  '{"type":"system","subtype":"model_refusal_no_fallback","uuid":"s-3","timestamp":"2030-01-02T03:00:05.000Z","originalModel":"claude-test-large","content":"0xREFUSEDTEXT"}'
out=$(echo "to the dialog 0xD1A1" | sender send paused-s 2>&1); rc=$?
check "send: exit 8 to a session paused on the dialog" 8 "$rc"
has "send: says to press no key" "$out" "paused on Claude Code's \`Session paused\` dialog" "Press no key"
lacks "send: nothing of the conversation in the output" "$out" "0xREFUSEDTEXT"
lacks "send: nothing typed into the paused session" "$(received paused-s)" "0xD1A1"
check "send: the paused session is left idle" idle "$(agent_field paused-s agent_status)"
echo "at the prompt 0xA7A7" | sender send prompt-s >/dev/null; rc=$?
check "send: exit 0 to a session at its prompt after a stop" 0 "$rc"
has "send: the session at its prompt gets the message" "$(received prompt-s)" "0xA7A7"
# Their workspaces closed, so later suites find only the agents they expect.
for name in paused-s prompt-s; do
  "${S[@]}" workspace close "$(agent_field "$name" workspace_id)" >/dev/null
done
check "send: paused-s and prompt-s are gone" "agent_not_found agent_not_found" \
  "$(agent_field paused-s agent_status) $(agent_field prompt-s agent_status)"
