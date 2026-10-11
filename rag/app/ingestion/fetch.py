"""SSRF-safe URL fetching for the "scrape a URL" knowledge source.

Defences:
* only http/https, no credentials in the URL, standard ports only;
* the hostname is resolved first and every address must be public;
* after connecting, the peer address is checked again (DNS-rebinding guard);
* redirects are followed manually (max 5) and each hop is re-validated;
* response size and time are capped; only HTML, text and PDF are accepted.
"""

from __future__ import annotations

import asyncio
import ipaddress
import socket
from dataclasses import dataclass
from urllib.parse import urljoin, urlsplit

import httpx

ALLOWED_PORTS = {None, 80, 443, 8080, 8443}
MAX_REDIRECTS = 5
USER_AGENT = "PeakChatBot/1.0 (+knowledge-base import)"


class FetchError(Exception):
    def __init__(self, message: str, permanent: bool = True):
        super().__init__(message)
        self.permanent = permanent


@dataclass
class Fetched:
    url: str
    content_type: str
    data: bytes


def _is_public(ip: ipaddress.IPv4Address | ipaddress.IPv6Address) -> bool:
    if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped:
        ip = ip.ipv4_mapped
    return ip.is_global and not (ip.is_multicast or ip.is_reserved or ip.is_loopback or ip.is_link_local)


async def _resolve_public(host: str, port: int) -> None:
    try:
        ip = ipaddress.ip_address(host)
        addrs = [ip]
    except ValueError:
        try:
            infos = await asyncio.get_running_loop().getaddrinfo(host, port, type=socket.SOCK_STREAM)
        except socket.gaierror as exc:
            raise FetchError(f"Could not resolve {host}") from exc
        addrs = [ipaddress.ip_address(info[4][0]) for info in infos]
    if not addrs or not all(_is_public(a) for a in addrs):
        raise FetchError("URL points to a private or reserved address")


def validate_url(url: str) -> tuple[str, int]:
    parts = urlsplit(url)
    if parts.scheme not in ("http", "https") or not parts.hostname:
        raise FetchError("Only http(s) URLs are supported")
    if parts.username or parts.password:
        raise FetchError("URLs with credentials are not allowed")
    if parts.port not in ALLOWED_PORTS:
        raise FetchError("Port not allowed")
    host = parts.hostname.lower()
    if host == "localhost" or host.endswith((".localhost", ".local", ".internal")):
        raise FetchError("URL points to a private host")
    return host, parts.port or (443 if parts.scheme == "https" else 80)


def _peer_ip(response: httpx.Response) -> str | None:
    stream = response.extensions.get("network_stream")
    if stream is None:
        return None
    addr = stream.get_extra_info("server_addr")
    return addr[0] if addr else None


async def fetch_url(url: str, max_bytes: int, timeout: float, trust_env: bool = False) -> Fetched:
    async with httpx.AsyncClient(
        follow_redirects=False,
        timeout=httpx.Timeout(timeout, connect=5.0),
        headers={"User-Agent": USER_AGENT, "Accept": "text/html,application/xhtml+xml,text/plain,application/pdf;q=0.9"},
        trust_env=trust_env,
    ) as client:
        current = url
        for _ in range(MAX_REDIRECTS + 1):
            host, port = validate_url(current)
            await _resolve_public(host, port)
            try:
                async with client.stream("GET", current) as resp:
                    peer = _peer_ip(resp)
                    if peer and not trust_env and not _is_public(ipaddress.ip_address(peer)):
                        raise FetchError("URL resolved to a private address")
                    if resp.is_redirect:
                        location = resp.headers.get("location")
                        if not location:
                            raise FetchError("Redirect without location")
                        current = urljoin(current, location)
                        continue
                    if resp.status_code >= 400:
                        raise FetchError(f"Page returned HTTP {resp.status_code}", permanent=resp.status_code < 500)
                    ctype = resp.headers.get("content-type", "").split(";")[0].strip().lower()
                    if ctype not in ("text/html", "application/xhtml+xml", "text/plain", "text/markdown", "application/pdf"):
                        raise FetchError(f"Unsupported content type {ctype or 'unknown'}")
                    declared = resp.headers.get("content-length")
                    if declared and declared.isdigit() and int(declared) > max_bytes:
                        raise FetchError("Page is too large")
                    buf = bytearray()
                    async for chunk in resp.aiter_bytes():
                        buf.extend(chunk)
                        if len(buf) > max_bytes:
                            raise FetchError("Page is too large")
                    return Fetched(url=str(resp.url), content_type=ctype, data=bytes(buf))
            except httpx.TimeoutException as exc:
                raise FetchError("Timed out fetching the page", permanent=False) from exc
            except httpx.HTTPError as exc:
                raise FetchError(f"Could not fetch the page: {exc.__class__.__name__}", permanent=False) from exc
        raise FetchError("Too many redirects")
