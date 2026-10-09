# Sourced by inside.sh: `send` against the real herdr and a fake Claude,
# every outcome of `herdr agent prompt` mapped to its exit code.
sender() { env "${P}AGENT=judge-sender" "${P}ROLE=orchestra" "$T" --session judge "$@"; }

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

echo "hello" | env "${P}AGENT=judge-sender" "$T" --session no-such-session send fake >/dev/null 2>&1; rc=$?
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
