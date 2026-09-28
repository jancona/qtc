package client

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jancona/qtc/envelope"
)

// Help describes what a Terminal accepts.
const Help = `Type a message:
  @W1AW hello         send to a callsign
  #NET hello          post to a room
  hello               send to whoever you last wrote to
Commands:
  /join NET           join a room          /leave NET   leave a room
  /rooms              list your rooms      /to W1AW     set who plain text goes to
  /sync               fetch what you missed
  /help               this help            /quit        disconnect and exit`

// Terminal is the line-oriented interface of qtc chat and qtc-radio: it
// turns typed lines into Session calls and Session events into lines.
type Terminal struct {
	// Sess is the session typed lines act on.
	Sess *Session
	// Out prints one line.
	Out func(string)

	mu sync.Mutex
	to string // where plain text goes: a callsign or "#ROOM"
}

func (t *Terminal) printf(format string, a ...any) { t.Out(fmt.Sprintf(format, a...)) }

// Input acts on one typed line and reports whether to quit.
func (t *Terminal) Input(line string) (quit bool) {
	line = strings.TrimSpace(line)
	switch {
	case line == "":
	case line == "/quit" || line == "/q":
		return true
	case line == "/help":
		t.Out(Help)
	case line == "/sync":
		t.Sess.StartSync()
	case strings.HasPrefix(line, "/to "):
		to := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(line[4:])), "@")
		if !validTarget(to) {
			t.printf("%q is not a callsign or #ROOM", to)
			return false
		}
		t.setTo(to)
		t.printf("Plain text now goes to %s.", to)
	case strings.HasPrefix(line, "/"):
		t.roomCommand(line)
	case strings.HasPrefix(line, "#"):
		room, msg, _ := strings.Cut(line, " ")
		room = strings.ToUpper(room)
		if !validTarget(room) {
			t.printf("%q is not a room name", room)
			return false
		}
		t.setTo(room)
		t.send(room, strings.TrimSpace(msg))
	case strings.HasPrefix(line, "@"):
		call, msg, _ := strings.Cut(line[1:], " ")
		to := strings.ToUpper(call)
		if !validCallsign(to) {
			t.printf("%q is not a callsign", call)
			return false
		}
		t.setTo(to)
		if msg = strings.TrimSpace(msg); msg != "" {
			t.send(to, msg)
		} else {
			t.printf("Plain text now goes to %s.", to)
		}
	default:
		t.mu.Lock()
		to := t.to
		t.mu.Unlock()
		if to == "" {
			t.printf("Who to? Start with a callsign (@W1AW hello) or a room (#NET hello).")
			return false
		}
		t.send(to, line)
	}
	return false
}

func (t *Terminal) setTo(to string) {
	t.mu.Lock()
	t.to = to
	t.mu.Unlock()
}

func validCallsign(s string) bool {
	a, err := envelope.EncodeAddress(s)
	return err == nil && a.IsStandard() && !strings.ContainsAny(s, " ")
}

func validTarget(s string) bool {
	if name, ok := strings.CutPrefix(s, "#"); ok {
		_, err := envelope.RoomAddress(name)
		return err == nil
	}
	return validCallsign(s)
}

// parseRoomCommand parses /join, /leave, or /rooms.
func parseRoomCommand(line string) (envelope.RoomOp, []envelope.Address, error) {
	fields := strings.Fields(line)
	var op envelope.RoomOp
	switch strings.ToLower(fields[0]) {
	case "/join":
		op = envelope.OpJoin
	case "/leave":
		op = envelope.OpLeave
	case "/rooms":
		op = envelope.OpList
	default:
		return 0, nil, fmt.Errorf("unknown command %s; try /help", fields[0])
	}
	var rooms []envelope.Address
	for _, name := range fields[1:] {
		a, err := envelope.RoomAddress(strings.TrimPrefix(name, "#"))
		if err != nil {
			return 0, nil, fmt.Errorf("%q is not a room name", name)
		}
		rooms = append(rooms, a)
	}
	if op != envelope.OpList && len(rooms) == 0 {
		return 0, nil, fmt.Errorf("which room? For example: %s NET", fields[0])
	}
	return op, rooms, nil
}

func (t *Terminal) roomCommand(line string) {
	op, rooms, err := parseRoomCommand(line)
	if err != nil {
		t.Out(err.Error())
		return
	}
	if err := t.Sess.RoomRequest(op, rooms); errors.Is(err, ErrNotLinked) {
		t.Out("Not linked; not sent.")
	} else if err != nil {
		t.printf("Cannot send: %v", err)
	}
}

func (t *Terminal) send(to, text string) {
	if text == "" {
		return
	}
	if len(text) > envelope.MaxBodyUnsigned {
		t.printf("Message too long: %d bytes, the limit is %d.", len(text), envelope.MaxBodyUnsigned)
		return
	}
	var dst envelope.Address
	var err error
	if name, ok := strings.CutPrefix(to, "#"); ok {
		dst, err = envelope.RoomAddress(name)
	} else {
		dst, err = envelope.EncodeAddress(to)
	}
	if err != nil {
		t.printf("Cannot send to %s: %v", to, err)
		return
	}
	if _, err := t.Sess.SendMsg(dst, text); errors.Is(err, ErrNotLinked) {
		t.Out("Not linked; message not sent.")
	} else if err != nil {
		t.printf("Cannot send: %v", err)
	}
}

// Show prints a session event, if it is one worth a line.
func (t *Terminal) Show(ev Event) {
	if line := Format(ev); line != "" {
		t.Out(line)
	}
}

func stamp() string { return time.Now().Format("15:04") }

// Format renders a session event as a line of chat, or "" for none.
func Format(ev Event) string {
	switch ev.Kind {
	case EventMessage:
		e := ev.Env
		m, _ := e.Msg()
		when := stamp()
		if ts := e.Timestamp(); ts != 0 {
			tm := time.Unix(int64(ts), 0)
			when = tm.Format("15:04")
			if time.Since(tm) > 12*time.Hour {
				when = tm.Format("Jan 2 15:04")
			}
		}
		switch {
		case ev.FromNode:
			return fmt.Sprintf("%s * %s", when, m.Body())
		case e.Destination().IsRoom():
			return fmt.Sprintf("%s %s %s: %s", when, e.Destination(), e.Source(), m.Body())
		}
		return fmt.Sprintf("%s %s: %s", when, e.Source(), m.Body())
	case EventReceipt:
		rc, _ := ev.Env.Rcpt()
		what := "your message"
		if ev.Text != "" {
			what = fmt.Sprintf("%q", ev.Text)
		}
		switch rc.Status() {
		case envelope.StatusDelivered:
			return fmt.Sprintf("%s * %s received %s", stamp(), ev.Env.Source(), what)
		case envelope.StatusTransmitted:
			return fmt.Sprintf("%s * %s sent on the air by %s", stamp(), what, ev.Env.Source())
		case envelope.StatusExpired:
			return fmt.Sprintf("%s * %s expired undelivered", stamp(), what)
		case envelope.StatusRejected:
			return fmt.Sprintf("%s * %s not sent: %s", stamp(), what, rc.Note())
		}
		return ""
	case EventRoomReply:
		r, _ := ev.Env.Room()
		var names []string
		for _, a := range r.Rooms() {
			names = append(names, a.String())
		}
		switch {
		case r.Op() == envelope.OpRefused && len(names) > 0:
			return fmt.Sprintf("%s * refused %s: %s", stamp(), strings.Join(names, " "), r.Note())
		case r.Op() == envelope.OpRefused:
			return fmt.Sprintf("%s * refused: %s", stamp(), r.Note())
		case ev.Op == envelope.OpList && len(names) == 0:
			return fmt.Sprintf("%s * no rooms", stamp())
		case ev.Op == envelope.OpList:
			return fmt.Sprintf("%s * rooms: %s", stamp(), strings.Join(names, " "))
		case ev.Op == envelope.OpJoin:
			return fmt.Sprintf("%s * joined", stamp())
		}
		return fmt.Sprintf("%s * left", stamp())
	case EventNotSent:
		return fmt.Sprintf("%s * not sent, the node did not answer: %q", stamp(), ev.Text)
	case EventSyncRefused:
		return fmt.Sprintf("%s * sync refused: %s", stamp(), ev.Text)
	}
	return ""
}
