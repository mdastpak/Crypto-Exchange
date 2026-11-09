# GeoIP: the country of a client

The server can find the country of each client ([#284](https://github.com/mdastpak/Crypto-Exchange/issues/284)), so that a visitor is offered the domain and language of their country ([#285](https://github.com/mdastpak/Crypto-Exchange/issues/285), [docs/i18n.md](i18n.md)). It is off by default (`geoip.enabled: false`); then no request has a country, and every visitor gets the default site. The settings are in [docs/configuration.md](configuration.md#geoip).

The country is a suggestion, never a security or compliance decision on its own: addresses move between countries, VPNs and mobile carriers hide them, and the databases are days or weeks old.

## Sources

`internal/geoip.Resolver` answers with an ISO 3166-1 alpha-2 code, such as `IR`, or no country, and the source of the answer. It tries, in this order:

1. **Overrides** (`geoip.overrides`): fixed countries for addresses or networks, to try the flow locally. Release mode refuses them.
2. **Edge header** (`geoip.header`): the country that a CDN or load balancer in front of the server already found. The header is trusted only when the TCP peer of the request is one of `server.trusted_proxies`; a client that sends the header itself is ignored. The usual headers:

   | Edge | Header |
   | --- | --- |
   | Cloudflare (IP Geolocation on) | `CF-IPCountry` |
   | Amazon CloudFront (with the header in the origin request policy) | `CloudFront-Viewer-Country` |
   | Google Cloud Load Balancing (custom request header `{client_region}`) | the header you name, such as `X-Client-Region` |
   | nginx with `ngx_http_geoip2_module` | the header you set, such as `X-Country-Code` |

   Values that name no country, such as `XX` (unknown), `T1` (Tor), or the continents `EU` and `AP` of older databases, count as none, and the database decides.
3. **Database** (`geoip.database_path`): a MaxMind-format (`.mmdb`) country or city database, looked up by the client address that Gin resolves through the trusted proxies, like the rate limits. The `country` of the record answers, or its `registered_country` for networks without one, such as anycast networks.

Private, loopback, link-local, shared (CGNAT, `100.64.0.0/10`), multicast, and unspecified addresses never have a country.

## Getting a database

Any MaxMind DB format country or city database works:

| Database | Licence | Updates | How to get it |
| --- | --- | --- | --- |
| [GeoLite2-Country](https://dev.maxmind.com/geoip/geolite2-free-geolocation-data) (MaxMind) | GeoLite2 EULA: free account, attribution required, keep it updated | twice a week | [`geoipupdate`](https://github.com/maxmind/geoipupdate) with an account ID and licence key |
| GeoIP2-Country (MaxMind) | commercial | weekly | `geoipupdate` |
| [DB-IP Lite Country](https://db-ip.com/db/lite.php) | CC BY 4.0: attribution required | monthly | download `dbip-country-lite-YYYY-MM.mmdb.gz` |

The databases are not in the repository and not in the image: they have licences of their own, and they change more often than the server. The attribution goes on the website's legal page.

## Deployment

Each instance reads its own copy of the file into memory, so the instances share no state. Two common layouts:

- **A volume per node, updated by `geoipupdate`**: run `geoipupdate` on a schedule (for example the `maxmindinc/geoipupdate` container with `GEOIPUPDATE_FREQUENCY=72`) into a volume that the server mounts read-only, and set `GEOIP_DATABASE_PATH` to the file in it. `geoipupdate` replaces the file in one rename.
- **The edge only**: behind Cloudflare or CloudFront, set `GEOIP_HEADER` and no database. Nothing runs per node.

Both can be combined: the header answers for requests through the edge, and the database for the others, such as health checks from inside the network (which have private addresses, so no country) or a second edge without the header.

Every `geoip.reload_interval_seconds` (300), each instance checks whether the file changed and loads it without a restart. The new database is loaded fully before it replaces the old one, so lookups never see half a file; a new file that does not load keeps the loaded one, and the error is logged and reported by the readiness check. Instances whose copies are a few days apart may disagree on an address that moved country; that only changes a suggestion.

At start, a database that does not load stops the server in release mode, because every visitor would otherwise silently get the default site; in development the server starts without it and logs a warning.

## Health

When GeoIP is enabled with a database, `/system/ready` checks it as an optional dependency, `geoip`: it fails when the database is not loaded, when its last reload failed, or when it is older than `geoip.max_age_days` (45) by its build date. The instance then answers `200` with the status `degraded`; it stays in service, since a missing country only means the default site.

## Privacy

The country is found per request and kept on that request only. It is not stored or logged.
