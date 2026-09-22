// Command qtc is the QTC command-line tool: decode envelopes, manage node
// keys, check the fixtures, talk to a mailbox node, and send messages.
//
// Usage:
//
//	qtc decode [-key file|-pubhex hex] [-json] <hex|base64|->
//	qtc keys gen <file> | qtc keys id <file>
//	qtc fixtures [file]
//	qtc store put|query|watch -peer <multiaddr> ...
//	qtc send -from CALL -to CALL|#ROOM -body TEXT [-rcpt] [-ttl MIN] [-admin host:port] [-json]
package main

import (
	"fmt"
	"os"
)

var commands = map[string]struct {
	run  func(args []string) error
	help string
}{
	"decode":   {runDecode, "parse an envelope given as hex, base64, or - for stdin"},
	"keys":     {runKeys, "gen <file> creates a node key; id <file> prints its peer ID and public key"},
	"fixtures": {runFixtures, "check docs/qtc-fixtures.json (or the given file) against the envelope package"},
	"store":    {runStore, "put, query, or watch a mailbox node over libp2p"},
	"send":     {runSend, "build a MSG and print it or hand it to a running station's admin interface"},
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "help" {
		usage()
		os.Exit(2)
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "qtc: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err := cmd.run(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "qtc %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: qtc <command> [flags]\n\ncommands:")
	for _, name := range []string{"decode", "keys", "fixtures", "store", "send"} {
		fmt.Fprintf(os.Stderr, "  %-9s %s\n", name, commands[name].help)
	}
}
