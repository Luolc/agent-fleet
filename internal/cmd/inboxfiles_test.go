package cmd

import (
	"os"
	"strings"
	"testing"
)

const filesEvent = `{"msg_id":"m1","payload":{"type":"message","thread":"C1/1.0","text":"see these","user":"U0ABC","ts":"1.5",
"files":[
{"id":"F0IMG1","name":"shot.png","mimetype":"image/png","size":10,"url":"https://files.example.test/shot","path":"/var/cache/fednet/F0IMG1-shot.png"},
{"id":"F0DOC2","name":"notes.txt","mimetype":"text/plain","size":20,"url":"https://files.example.test/notes"},
{"id":"F0BIG3","name":"big.pdf","mimetype":"application/pdf","size":30,"url":"https://files.example.test/big","error":"file too large"}]}}`

func eventOf(t *testing.T, raw string) *event {
	t.Helper()
	path := t.TempDir() + "/e.json"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := readEvent(path)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAMessageWithFilesListsEachOne(t *testing.T) {
	msg := inboxMessage(eventOf(t, filesEvent), "", "/run/example/fednet.sock")
	const cmd = "`fednet client fetch-file -socket /run/example/fednet.sock "
	want := "Message in thread C1/1.0 from U0ABC at 1.5:\n\nsee these\n\n" +
		"Files attached to the message (open the path with the Read tool to see the file; a " +
		"`file: <name> <link>` line in the text is a Slack link that needs a login, so do not use it, go by the path):\n" +
		"- shot.png (image/png, id F0IMG1): path /var/cache/fednet/F0IMG1-shot.png\n" +
		"- notes.txt (text/plain, id F0DOC2): not fetched; to get a local path run " + cmd + "F0DOC2`, which prints it, then Read it\n" +
		"- big.pdf (application/pdf, id F0BIG3): not fetched, error: file too large; to try again run " + cmd +
		"F0BIG3`, which prints the path, then Read it\n" +
		"Fetched files are kept on this machine for 7 days by default; when a path is gone, fetch the file again with " +
		cmd + "<id>`. A file over 200 MiB is refused: then tell the person to share it another way.\n"
	if got := msg.body(); got != want {
		t.Errorf("body:\n%q\nwant\n%q", got, want)
	}
}

func TestAFileWithAPathGetsNoCommandOfItsOwn(t *testing.T) {
	msg := inboxMessage(eventOf(t, filesEvent), "", "/s")
	for _, line := range strings.Split(msg.body(), "\n") {
		if strings.HasPrefix(line, "- shot.png") && strings.Contains(line, "fetch-file") {
			t.Errorf("a file with a path gets a fetch-file command: %q", line)
		}
	}
}

func TestARelayedMessageListsItsFilesToo(t *testing.T) {
	e := eventOf(t, strings.Replace(filesEvent, `"type":"message",`, `"type":"message","trigger":"relay","from_client":"m1",`, 1))
	got := inboxMessage(e, "", "/s").body()
	if !strings.HasPrefix(got, "[RELAY from m1]\n") || !strings.Contains(got, "path /var/cache/fednet/F0IMG1-shot.png") {
		t.Errorf("relay body: %q", got)
	}
}

func TestAMessageWithoutFilesIsUnchanged(t *testing.T) {
	e := eventOf(t, `{"msg_id":"m2","payload":{"type":"message","thread":"C1/1.0","text":"hi","user":"U0ABC","ts":"1.5"}}`)
	want := "Message in thread C1/1.0 from U0ABC at 1.5:\n\nhi\n"
	if got := inboxMessage(e, "", "/s").body(); got != want {
		t.Errorf("body %q, want %q", got, want)
	}
}
