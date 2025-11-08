# SIP to RTSP Bridge

A Go application that bridges SIP calls to RTSP streams, allowing multiple extensions with individual audio streams.

## Features

- **Multiple Extensions**: Configure multiple SIP extensions, each with its own RTSP stream
- **Static RTSP Streams**: All configured extensions have active RTSP streams that serve silence when no call is active
- **Call Timeout**: Optional maximum call duration to automatically terminate long-running calls

## Building

```bash
go build
```

## Configuration

### Creating a Configuration File

To create a default configuration file:

```bash
./sip2rtsp --create-config
```

This creates `config.json` with default settings.

### Configuration Options

Edit `config.json` to customize settings:

```json
{
  "sip_listen_addr": "0.0.0.0",
  "sip_port": 5060,
  "rtsp_listen_addr": ":9554",
  "log_level": "info",
  "call_timeout_seconds": 3600,
  "extensions": [
    {
      "number": "100",
      "one_call_only": true,
      "sip_registration": {
        "enabled": false,
        "server": "sip.example.com:5060",
        "username": "100",
        "password": "password100",
        "display_name": "Extension 100",
        "expires": 3600
      }
    },
    {
      "number": "101",
      "one_call_only": false,
      "sip_registration": {
        "enabled": true,
        "server": "sip.yourpbx.com:5060",
        "username": "101",
        "password": "secret101",
        "display_name": "Extension 101",
        "expires": 3600
      }
    }
  ]
}
```

**Global Configuration Parameters:**

- `sip_listen_addr`: IP address to listen for SIP connections (default: "0.0.0.0")
- `sip_port`: SIP port to listen on (default: 5060)
- `rtsp_listen_addr`: RTSP server address (default: ":9554")
- `log_level`: Logging level - "debug", "info", "warn", or "error" (default: "info")
- `call_timeout_seconds`: Maximum call duration in seconds for all extensions (0 = no timeout, default: 3600)

**Per-Extension Configuration:**

Each extension in the `extensions` array supports the following settings:

- `number`: Extension number (e.g., "100", "101")
- `one_call_only`: If true, only one call at a time is allowed for this extension (default: false)
- `sip_registration`: SIP registration settings for this extension (optional)
  - `enabled`: Enable/disable SIP registration for this extension (default: false)
  - `server`: SIP server address for registration (e.g., "sip.example.com:5060")
  - `username`: SIP username for this extension
  - `password`: SIP password for this extension
  - `display_name`: Display name for registration (optional)
  - `expires`: Registration expiration time in seconds (default: 3600)

## Running

### With Default Config Location

```bash
./sip2rtsp
```

### With Custom Config File

```bash
./sip2rtsp --config /path/to/config.json
```

## Usage

### RTSP Streams

Once the application starts, all configured extensions will have RTSP streams available:

- Extension 100: `rtsp://localhost:9554/100`
- Extension 101: `rtsp://localhost:9554/101`

These streams are **always available** and will serve µ-law silence when no call is active.

### Making a SIP Call

When you make a SIP call to an extension (e.g., `sip:100@your-server`):

1. The application validates the extension is configured
2. If `one_call_per_extension` is enabled, it checks if the extension is already in use
3. The call audio is routed to the corresponding RTSP stream
4. Multiple RTSP clients can connect to the same stream simultaneously
5. When the call ends (BYE or timeout), the RTSP stream continues serving silence