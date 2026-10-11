import pytest

from app.ingestion.fetch import FetchError, _resolve_public, validate_url


@pytest.mark.parametrize("url", [
    "ftp://example.com/x", "file:///etc/passwd", "https://user:pw@example.com/", "http://localhost:8080/",
    "http://metadata.google.internal/computeMetadata/v1/", "https://example.com:6379/", "http:///nohost",
])
def test_rejects_bad_urls(url):
    with pytest.raises(FetchError):
        validate_url(url)


def test_accepts_public_urls():
    assert validate_url("https://Example.com/faq?x=1") == ("example.com", 443)
    assert validate_url("http://example.com:8080/") == ("example.com", 8080)


@pytest.mark.parametrize("host", ["127.0.0.1", "10.0.0.5", "172.16.3.4", "192.168.1.1", "169.254.169.254",
                                  "::1", "fd00::1", "::ffff:127.0.0.1", "0.0.0.0", "100.64.0.1"])
async def test_private_addresses_blocked(host):
    with pytest.raises(FetchError):
        await _resolve_public(host, 443)


async def test_public_ip_allowed():
    await _resolve_public("93.184.216.34", 443)
