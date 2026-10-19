// `fleet thread list`: the threads of a channel, whichever machine owns
// them, so an agent finds the key of another machine's thread.

package cmd

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// ThreadListAbout and ThreadListLongAbout are the help texts of `thread
// list`.
const (
	ThreadListAbout     = "List the threads of a channel, whichever machine owns them (read-only)"
	ThreadListLongAbout = "List the threads of a channel, whichever machine owns them (read-only).\n\n" +
		"For a thread of another machine whose key you do not have, such as one to answer with `fleet thread post " +
		"--deliver --thread <THREAD>`. --channel is the channel's name without the #; fleet takes its id from " +
		"fednet's routes (`fednet client routes -json`) and asks the hub for its threads (`fednet client threads " +
		"-json`, fednet v0.10.0 or later on this machine and on the hub), --limit passed on as `-limit` (fednet's " +
		"default is 20). The fednet socket is the scope's (`fednet.socket`); any caller may run it.\n\n" +
		"One line per thread, newest first, tab-separated: the thread key `CHANNEL/TS`, the fednet client that " +
		"owns it, and when it started (RFC 3339, UTC). A channel without threads prints nothing.\n\n" +
		"Exit: 0; 1 when --channel is not a Slack channel's name, fednet's routes name no such channel, --limit " +
		"is not a positive number, or the socket is not configured; 5 when fednet cannot be run, fails (a client " +
		"or hub older than v0.10.0, its stderr's first line says which) or answers with what is not its JSON, " +
		"or the settings fail."
)

// ThreadListArgs are the arguments of `thread list`: the channel's name,
// and the most threads to list (0 for fednet's default).
type ThreadListArgs struct {
	Channel string
	Limit   int
}

// ThreadList runs `thread list`.
func ThreadList(args ThreadListArgs) (exit.Code, error) {
	if !slackChannel.MatchString(args.Channel) {
		return 0, exit.Refusedf("--channel %q is not a Slack channel's name, such as repo-example: lower-case "+
			"letters, digits, - and _, without the #", args.Channel)
	}
	scope, err := identity.Scope()
	if err != nil {
		return 0, err
	}
	cfg, err := config.LoadScope(scope)
	if err != nil {
		return 0, err
	}
	if cfg.FednetSocket == "" {
		return 0, exit.Refusedf("fednet.socket is not configured for scope %s, so fleet cannot ask fednet", scope)
	}
	routes, err := fednet.Channels(cfg.FednetSocket)
	if err != nil {
		return 0, err
	}
	route, ok := routes[args.Channel]
	if !ok {
		names := strings.Join(slices.Sorted(maps.Keys(routes)), ", ")
		if names == "" {
			names = "none"
		}
		return 0, exit.Refusedf("fednet's routes name no channel #%s; the channels they name: %s", args.Channel, names)
	}
	threads, err := fednet.Threads(cfg.FednetSocket, route.Channel, args.Limit)
	if err != nil {
		return 0, err
	}
	for _, t := range threads {
		fmt.Fprintln(os.Stdout, strings.Join([]string{t.Thread, t.Owner, t.StartedAt}, "\t"))
	}
	return exit.Ok, nil
}
