package cmd

import (
	"os"
	"strings"
	"testing"
)

// filesEvent is a message with files as fednet hands it over: the text
// ends with a `file: <name> <permalink>` line for each file, the permalink
// ending in the file's name.
const filesEvent = `{"msg_id":"m1","payload":{"type":"message","thread":"C1/1.0","text":"see these\nfile: shot.png https://ws.example.test/files/U0ABC/F0IMG1/shot.png\nfile: notes.txt https://ws.example.test/files/U0ABC/F0DOC2/notes.txt\nfile: big.pdf https://ws.example.test/files/U0ABC/F0BIG3/big.pdf","user":"U0ABC","ts":"1.5",
"files":[
{"id":"F0IMG1","name":"shot.png","mimetype":"image/png","size":10,"url":"https://ws.example.test/files/U0ABC/F0IMG1/shot.png","path":"/var/cache/fednet/F0IMG1-shot.png"},
{"id":"F0DOC2","name":"notes.txt","mimetype":"text/plain","size":20,"url":"https://ws.example.test/files/U0ABC/F0DOC2/notes.txt"},
{"id":"F0BIG3","name":"big.pdf","mimetype":"application/pdf","size":30,"url":"https://ws.example.test/files/U0ABC/F0BIG3/big.pdf","error":"file too large"}]}}`

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
	want := "Message in thread C1/1.0 from U0ABC at 1.5:\n\nsee these\nfile: shot.png `https://ws.example.test/files/U0ABC/F0IMG1/shot.png`\n" +
		"file: notes.txt `https://ws.example.test/files/U0ABC/F0DOC2/notes.txt`\nfile: big.pdf `https://ws.example.test/files/U0ABC/F0BIG3/big.pdf`\n\n" +
		"Files attached to the message (open the path with the Read tool to see the file; a " +
		"`file: <name> <link>` line in the text is a Slack link that needs a login, so do not use it, go by the path):\n" +
		"- shot.png (image/png, id F0IMG1): path `/var/cache/fednet/F0IMG1-shot.png`\n" +
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
	if !strings.HasPrefix(got, "[RELAY from m1]\n") || !strings.Contains(got, "path `/var/cache/fednet/F0IMG1-shot.png`") {
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
