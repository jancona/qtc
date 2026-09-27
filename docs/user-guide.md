# QTC user guide

QTC is text messaging for M17. You send a message to a callsign, and it reaches that station wherever they are on the QTC network, now or later, through whichever hotspot last heard them. There are also rooms: named group conversations that anyone can join.

This guide is for using QTC from an M17 radio or from the `qtc chat` program. To set up a hotspot, see the [operator guide](operator-guide.md).

QTC is in an **invite-only test**. Messages are not private and callsigns are not verified; see [what to keep in mind](#what-to-keep-in-mind).

## From a radio

You need an M17 radio that can send and receive SMS, and a hotspot running QTC. The hotspot's gateway links to reflector `M17-QTC`, module A. Voice works exactly as before, passing through to the hotspot's usual reflector. Text messages go into QTC.

### Being heard

QTC delivers your messages to the hotspot that last heard you. You're heard whenever you transmit through a QTC hotspot, by voice or SMS. A quick key-up (a kerchunk) is enough.

Transmissions to **ECHO** or **INFO** don't count: the hotspot's gateway answers those itself, and QTC never sees them.

### Sending a message

Send an SMS to the other station's callsign, just as you would on a reflector. It's delivered to them whether they're on your hotspot, on another QTC hotspot, or off the air. If they're off the air, it waits for them.

- To reach all of someone's radios, use their base callsign: `W1XYZ`.
- To reach one radio, include its suffix, e.g. `W1XYZ 7`.

A message can be up to about 800 characters, though your radio may allow less.

### Rooms

A room is a group conversation, named with up to 8 letters, digits, or hyphens, such as `NET` or `MAINE`. You talk to rooms through the hotspot itself, by sending SMS to the **hotspot's callsign**. By default that's the operator's callsign with the module letter Q, e.g. `N1ADJ Q`. Your radio can send it with a single space.

| Send to the hotspot's callsign | What happens |
|---|---|
| `/rooms` | The hotspot replies with the rooms you're in, e.g. `rooms: MAINE N1ADJ` |
| `/join MAINE` | Joins the room. The reply is `joined MAINE`. You can join several at once: `/join MAINE NET` |
| `/leave MAINE` | Leaves the room. The reply is `left MAINE` |
| `#MAINE net starts at 7` | Posts "net starts at 7" to room MAINE, and joins you to it if you weren't already |
| `anyone around?` | Posts to the hotspot's **local room** (see below) |

Room messages arrive as SMS from the person who posted them, with the room name at the start, e.g. `#MAINE net starts at 7`. You don't receive your own posts.

Each hotspot has a **local room**, named after its operator's callsign (`N1ADJ` for hotspot `N1ADJ Q`): everyone that hotspot has heard. You're joined to it automatically when the hotspot hears you. If you `/leave` it, you stay out even when you're heard again, until you post to it or join it yourself.

Rooms you join, or post to, follow you: they're stored with your messages, so they're the same on any QTC hotspot. Local rooms are the exception: you're in the local room of whichever hotspot is hearing you.

### When you've been away

A hotspot only transmits messages to a radio it has heard within the **last hour**. That way, messages sent while you're switched off or out of range wait for you instead of being transmitted to no one.

When you're heard again, the hotspot sends what's waiting:

- the **10 most recent** messages, oldest first;
- preceded, if there were more, by a note from the hotspot such as `7 older messages not sent`.

Messages are kept for a week, unless the sender chose a different lifetime.

**If you've been listening without transmitting for over an hour,** new messages wait too, until you next key up. A kerchunk when you sit down at the radio is a good habit.

Moving between hotspots is fine: messages already sent to you on one hotspot aren't sent again on another.

### Delivery isn't guaranteed

A text message over the air goes out once, and your radio can't confirm it arrived. Now and then a message is lost to a weak or noisy signal, and QTC can't tell. If something matters, ask for a reply.

## From a computer: `qtc chat`

If you don't have a radio, or want a keyboard, `qtc chat` connects to a QTC hotspot or public station over the internet as a chat client. You need:

- The `qtc` program, from the `qtcd` package or a release tarball on the [releases page](https://github.com/jancona/qtc/releases). Linux builds only for now.
- The address of a QTC station that accepts internet clients, and your callsign on its allow list. Ask the station's operator.

```
qtc chat -callsign N1ADJ -node qtc.example.net
```

`-node` takes `host` or `host:port` (the port defaults to 17000). If you have an `M17Hosts.txt` that lists the station, use `-reflector NAME -hosts M17Hosts.txt` instead.

| Type | What happens |
|---|---|
| `W1XYZ: hello` | Sends to a callsign |
| `#MAINE hello` | Posts to a room |
| `hello` | Sends to whoever you last wrote to, callsign or room |
| `/to W1XYZ` | Sets who plain text goes to |
| `/join MAINE`, `/leave MAINE`, `/rooms` | Room commands, answered by the station |
| `/help`, `/quit` | Help, and disconnect |

Incoming messages show the time and the sender. Room messages show the room first, and replies from the station itself start with `*`:

```
19:42 W1XYZ: are you on the net tonight?
19:43 #MAINE K1ABC: net starts at 7
19:43 * joined MAINE
```

While `qtc chat` is connected, the station counts you as present, so messages reach you as they arrive. When you connect after being away, you get what's waiting, as described [above](#when-youve-been-away). If the station refuses the connection, your callsign probably isn't on its allow list.

## What to keep in mind

- **Messages are not private.** Every station that stores or forwards a message can read it, and amateur radio rules don't allow encryption anyway. Don't send anything you wouldn't say on the air.
- **Callsigns are not verified.** QTC believes the callsign a radio or client gives. A message claiming to be from someone might not be.
- **This is a test.** Things may change between releases, and messages may occasionally be lost. Please report problems to your hotspot's operator, or at [github.com/jancona/qtc/issues](https://github.com/jancona/qtc/issues).
