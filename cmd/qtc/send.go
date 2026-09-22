package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/jancona/qtc/envelope"
)

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	from := fs.String("from", "", "source callsign, e.g. \"N1ADJ  H\"")
	to := fs.String("to", "", "destination callsign or #ROOM")
	body := fs.String("body", "", "message text")
	rcpt := fs.Bool("rcpt", false, "request a delivery receipt")
	ttl := fs.Uint("ttl", 1440, "time to live in minutes (0 live only, 65535 node default)")
	admin := fs.String("admin", "", "hand the message to a running station's admin interface at host:port instead of printing it")
	asJSON := fs.Bool("json", false, "print JSON with hex and base64 forms")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" {
		return errors.New("-from and -to are required")
	}
	if *ttl > 0xFFFF {
		return errors.New("-ttl must be 0 to 65535")
	}
	if *admin != "" {
		q := url.Values{"from": {*from}, "to": {*to}, "body": {*body}, "ttl": {strconv.Itoa(int(*ttl))}}
		if *rcpt {
			q.Set("rcpt", "1")
		}
		resp, err := http.Post("http://"+*admin+"/send?"+q.Encode(), "text/plain", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("station: %s: %s", resp.Status, out)
		}
		fmt.Print(string(out))
		return nil
	}
	src, err := envelope.EncodeAddress(*from)
	if err != nil {
		return err
	}
	dst, err := envelope.ParseAddress(*to)
	if err != nil {
		return err
	}
	nonce, err := envelope.NewNonce()
	if err != nil {
		return err
	}
	var flags byte
	if *rcpt {
		flags |= envelope.FlagRcptReq
	}
	e, err := envelope.BuildMsg(src, dst, uint32(time.Now().Unix()), uint16(*ttl), nonce, flags, *body)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]string{
			"id":     e.ID().String(),
			"hex":    hex.EncodeToString(e.Bytes()),
			"base64": base64.StdEncoding.EncodeToString(e.Bytes()),
			"text":   e.String(),
		})
	}
	fmt.Println(hex.EncodeToString(e.Bytes()))
	return nil
}
