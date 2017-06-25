### barvin-slack

Translate Slack "protocol" to some internal 'JSON over TCP' protocol; fanout messaging system for Slack - "The Connector".
Can connect components simply by tcp, no security just yet.
Why not Websockets? Well, a bit too complex for what we need, and also I'd rather have something out of the box for the most languages out there (i.e. TCP sockets), rather than relying on 3rd party implementations that sometimes work.

Connect the component using TCP sockets, on port 9191 (default).

### Protocol

The protocol is extremly simple, for now:

- register the component: `{"type": "register", "data": "my-fancy-component"}`;
- send messages from the slack connector to the components (since this is a fanout implementation, all components connected at a particular time, will get a copy of the message) using the same simple json format `{"type": "msg", "data": "some data in here"}`;
- the components can send messages to the connector, again using the same json format `{"type": "msg", "data": "some data from the component"}` - the connector will know what to do with that message based on the specified `type`;
- the component -> connector communication is based on a req-resp pattern - that means that for every message that the component will send to the connector, there will be a json response i.e. `{"type": "ok", "data": "message successfully sent"}`, `{"type": "error", "data": "some error reason"}`;
- connector -> components message types:
  - `msg` - normal message sent to the component
  - `ok` - reply sent back to the component when the request was successfully processed/handled
  - `error` - when there was an error related to the message itself, or something was wrong on the connector side (i.e. maybe Slack was down, and the message didn't get to the destination?)
- components -> connector message types:
  - `register` - register the current component - usually this message is being sent as the very first TCP connection
  - `msg` - a normal message that needs to end up in Slack
