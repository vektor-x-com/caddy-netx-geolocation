# caddy-netx-geolocation

A Caddy plugin that provides IP geolocation data using the [NET-X](https://net.vektor-x.com) network intelligence API.

Unlike traditional geolocation plugins that query external APIs on every request, this plugin fetches all data in bulk once per day and performs lookups entirely in-memory — zero external API calls per request.

## How it works

1. On startup, loads geolocation data from a local file (`.gob` format)
2. If no local file exists, performs a full bulk download from the NET-X API
3. Every day at a configured time, refreshes the data in the background
4. Each HTTP request gets an in-memory lookup (~9ns, zero allocations)
5. If the API is unavailable during refresh, the plugin continues operating with existing data

## Installation

Build Caddy with the plugin using [xcaddy](https://github.com/caddyserver/xcaddy):

```bash
xcaddy build --with github.com/whitebyte/caddy-netx-geolocation
```

## Configuration

### Caddyfile

```caddyfile
{
    order netx_geolocation before respond
}

example.com {
    netx_geolocation {
        # API base URL (optional, default: https://net.vektor-x.com)
        api_url https://net.vektor-x.com

        # Directory to store the local data file (optional, default: caddy AppDataDir)
        data_dir /var/lib/caddy/netx

        # Daily refresh time in HH:MM local time (optional, default: 03:00)
        refresh_time 03:00

        # Block/allow by country (2-letter ISO code)
        deny_countries RU CN
        # allow_countries US DE GB

        # Block/allow by organization name
        deny_orgs "Evil Corp" "Spam Inc"
        # allow_orgs "Google LLC" "Cloudflare Inc"

        # Block/allow by registry
        deny_registries apnic
        # allow_registries arin ripencc
    }

    respond "Hello from {netx_geo.country}" 200
}
```

### Directives

| Directive | Description | Default |
|-----------|-------------|---------|
| `api_url` | NET-X API base URL | `https://net.vektor-x.com` |
| `data_dir` | Directory for the local data file | Caddy AppDataDir |
| `refresh_time` | Daily refresh time (HH:MM, local time) | `03:00` |
| `allow_countries` | Only allow these country codes | (allow all) |
| `deny_countries` | Block these country codes | (deny none) |
| `allow_orgs` | Only allow these organization names | (allow all) |
| `deny_orgs` | Block these organization names | (deny none) |
| `allow_registries` | Only allow these registries | (allow all) |
| `deny_registries` | Block these registries | (deny none) |

Deny rules take precedence over allow rules.

### Placeholders

Available for use in other directives after `netx_geolocation` runs:

| Placeholder | Description |
|-------------|-------------|
| `{netx_geo.country}` | 2-letter ISO country code (e.g. `US`, `DE`) |
| `{netx_geo.registry}` | Regional Internet Registry (`arin`, `ripencc`, `apnic`, `lacnic`, `afrinic`) |
| `{netx_geo.org_name}` | Organization name |
| `{netx_geo.org_id}` | Organization ID |

If an IP is not found in the database, `country` and `registry` are set to `-`.

## Usage Examples

### Block traffic from specific countries

```caddyfile
{
    order netx_geolocation before respond
}

example.com {
    netx_geolocation {
        deny_countries RU CN KP
    }

    reverse_proxy localhost:3000
}
```

### Allow only specific countries

```caddyfile
example.com {
    netx_geolocation {
        allow_countries US CA GB DE FR
    }

    file_server
}
```

### Add geolocation headers to proxied requests

```caddyfile
example.com {
    netx_geolocation

    header_up X-Geo-Country {netx_geo.country}
    header_up X-Geo-Registry {netx_geo.registry}
    header_up X-Geo-Org {netx_geo.org_name}

    reverse_proxy localhost:8080
}
```

### Log visitor geolocation

```caddyfile
example.com {
    netx_geolocation

    log {
        output file /var/log/caddy/access.log
        format json {
            fields {
                geo_country {netx_geo.country}
                geo_org {netx_geo.org_name}
            }
        }
    }

    file_server
}
```

### Block by organization name

```caddyfile
example.com {
    netx_geolocation {
        deny_orgs "Known Spam Network" "Botnet Hosting LLC"
    }

    reverse_proxy localhost:3000
}
```

### Combine with Caddy's native IP blocking

```caddyfile
example.com {
    # Block specific IPs (Caddy built-in)
    @blocked_ips remote_ip 1.2.3.4 5.6.7.0/24
    respond @blocked_ips 403

    # Block by geolocation (this plugin)
    netx_geolocation {
        deny_countries CN RU
        deny_registries afrinic
    }

    reverse_proxy localhost:3000
}
```

### Use placeholders in responses

```caddyfile
example.com {
    netx_geolocation

    respond "Your country: {netx_geo.country}, Registry: {netx_geo.registry}, Org: {netx_geo.org_name}" 200
}
```

## Data Source

Geolocation data is provided by [NET-X](https://net.vektor-x.com), which aggregates IP range registration data from all five Regional Internet Registries (RIRs):

- **ARIN** — North America
- **RIPE NCC** — Europe, Middle East, Central Asia
- **APNIC** — Asia Pacific
- **LACNIC** — Latin America, Caribbean
- **AFRINIC** — Africa

The database contains ~572,000 IP range entries covering ~70,000 organizations.

## Performance

- Lookup time: **~9ns per request** (zero allocations)
- Memory: proportional to dataset (~572k entries)
- Local data file: ~38MB (gob-encoded)
- Daily refresh: ~78 seconds (rate-limited to respect API limits)
- Startup with existing data file: instant

## Resilience

- If the API is unreachable during daily refresh, existing data is preserved
- If the API is unreachable on first startup and no local file exists, the plugin starts with empty data (all lookups return `-`, no blocking occurs)
- Data file writes are atomic (temp file + rename) to prevent corruption
- Malformed CIDR entries from the API are skipped without affecting other entries

## License

ISC
