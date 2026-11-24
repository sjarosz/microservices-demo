#!/usr/bin/env python3
"""End-to-end check of the identity experience, driven through the BFF like a browser would (standard library only).

  BASE_URL=http://localhost:8080 python3 src/bff/e2e/e2e.py

It registers a new shopper at the identity provider's own sign-up page, so it expects the development provider
(Keycloak, kustomize/components/identity-dev-idp) or another provider whose sign-up form has the same field names.
Every run creates one account: e2e-<timestamp>@example.com.
"""
import html
import http.cookiejar
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get("BASE_URL", "http://localhost:8080").rstrip("/")
PRODUCT, PRODUCT2 = "OLJCESPC7Z", "66VCHSJNUP"
EMAIL, PASSWORD = f"e2e-{int(time.time())}@example.com", "Boutique-e2e-1!"
CARD = {"credit_card_number": "4432801561520454", "credit_card_expiration_month": "1",
        "credit_card_expiration_year": str(time.gmtime().tm_year + 2), "credit_card_cvv": "672"}
ADDRESS = {"street_address": "1 Identity Way", "zip_code": "10001", "city": "New York", "state": "NY",
           "country": "United States"}
GATEWAY = {"gateway", "__Host-gateway"}     # the PingGateway sidecar's session cookie, when it is deployed
failed = 0


class Browser:
    def __init__(self):
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))

    def request(self, url, data=None, headers=None):
        """Returns (status, final url, body, headers); redirects are followed like a browser does."""
        if url.startswith("/"):
            url = BASE + url
        body = urllib.parse.urlencode(data).encode() if data is not None else None
        req = urllib.request.Request(url, data=body, headers={"Origin": BASE, **(headers or {})})
        try:
            with self.opener.open(req, timeout=60) as r:
                return r.status, r.url, r.read().decode(errors="replace"), r.headers
        except urllib.error.HTTPError as e:
            return e.code, e.url, e.read().decode(errors="replace"), e.headers
        finally:
            if BASE.startswith("http://"):      # browsers send Secure cookies to http://localhost; this jar would not
                for c in self.jar:
                    c.secure = False

    def cookies(self):
        """The shop's cookies. Locally the provider shares the host name, so its cookies are told apart by path."""
        host = urllib.parse.urlsplit(BASE).hostname     # the jar files "localhost" under "localhost.local"
        return {c.name: c.value for c in self.jar if c.domain.lstrip(".") in (host, host + ".local") and c.path == "/"}


def check(name, ok, detail=""):
    global failed
    failed += not ok
    print(f"{'ok  ' if ok else 'FAIL'} {name}" + (f"  [{detail}]" if detail and not ok else ""))
    return ok


def form_action(page):
    m = re.search(r'<form[^>]*action="([^"]+)"', page)
    return html.unescape(m.group(1)) if m else None


def order_id(page):
    m = re.search(r"Confirmation #\s*</div>\s*<div[^>]*>\s*([0-9a-f-]{36})", page)
    return m.group(1) if m else None


def result(b, oid):
    s, _, body, _ = b.request(f"/bff/api/order/{oid}")
    return json.loads(body) if s == 200 else {}


def main():
    b = Browser()

    # --- anonymous browsing ------------------------------------------------------------------------------------
    s, _, page, _ = b.request("/")
    check("home is served to an anonymous shopper", s == 200 and "Hot Products" in page)
    check("anonymous: sign-in is offered, nobody is greeted", "Sign in" in page and "id-account" not in page)
    # With the PingGateway sidecar the browser also holds the gateway's opaque session cookie, from the first request.
    check("anonymous: no BFF session before it is needed", set(b.cookies()) - GATEWAY == {"shop_session-id"}, str(b.cookies()))
    forged = "eyJzaWduZWRfaW4iOnRydWUsImF1dGhfZW5hYmxlZCI6dHJ1ZSwidXNlciI6eyJnaXZlbl9uYW1lIjoiTWFsbG9yeSJ9fQ.AAAA"
    s, _, page, _ = b.request("/", headers={"X-Bff-Context": forged})
    check("a context header sent by the browser is ignored", "Mallory" not in page)
    b.request(f"/product/{PRODUCT}")
    s, _, page, _ = b.request("/")
    check("anonymous personalization: recently viewed row", "Recently viewed" in page)

    # --- the point of checkout ---------------------------------------------------------------------------------
    b.request("/cart", {"product_id": PRODUCT, "quantity": "2"})
    s, _, page, _ = b.request("/cart")
    check("cart asks how to check out", "How would you like to check out?" in page and "Place Order" not in page)
    s, _, page, _ = b.request("/cart?checkout=guest")
    check("guest checkout shows the one-time purchase form", "One-time purchase" in page and "Place Order" in page)
    check("guest form offers nothing to remember", "remember_payment" not in page)

    # --- one-time purchase -------------------------------------------------------------------------------------
    s, _, page, hdrs = b.request("/cart/checkout", {"email": "guest@example.com", **ADDRESS, **CARD})
    guest_order = order_id(page)
    check("guest order completes", s == 200 and "Your order is complete!" in page and guest_order, f"{s}")
    check("order report headers stop at the BFF", not [h for h in hdrs.keys() if h.lower().startswith("x-order")])
    guest = result(b, guest_order)
    check("guest order is claimable, nothing credited", guest.get("signed_in") is False
          and guest.get("claimable_points", 0) > 0, str(guest))

    # --- register (the provider's own sign-up page) ----------------------------------------------------------------
    s, url, page, _ = b.request("/auth/register?return_to=/account")
    check("register sends the shopper to the provider's sign-up page",
          not url.startswith(BASE) and "password-confirm" in page, url[:80])
    s, url, page, _ = b.request(form_action(page), {"email": EMAIL, "password": PASSWORD, "password-confirm": PASSWORD,
                                                    "firstName": "Eve", "lastName": "Tester"})
    check("after sign-up the shopper lands on the account page", url == BASE + "/account" and "Hi, Eve" in page, url[:80])
    check("welcome bonus and guest order points are announced",
          "100 bonus points" in page and "from your guest order were added" in page)
    pts = re.search(r'id-big">(\d+) points', page)
    check("balance = welcome bonus + claimed guest order",
          pts and int(pts.group(1)) == 100 + guest["claimable_points"], pts.group(0) if pts else "no balance")
    check("the notice is shown once", "100 bonus points" not in b.request("/account")[2])
    names = set(b.cookies())
    check("browser holds only the session cookie and the shop's own",
          names and names <= {"bff", "__Host-bff", "shop_session-id", "shop_currency"} | GATEWAY, str(names))
    check("no token reaches the page", "eyJ" not in page)

    # --- member checkout: remember card and address ------------------------------------------------------------
    b.request("/cart", {"product_id": PRODUCT2, "quantity": "1"})
    s, _, page, _ = b.request("/cart")
    check("member cart skips the question and is prefilled",
          "How would you like to check out?" not in page and f'value="{EMAIL}"' in page and "pt per $1" in page)
    s, _, page, _ = b.request("/cart/checkout", {"email": EMAIL, **ADDRESS, **CARD, "saved_payment_id": "new",
                                                 "remember_payment": "on", "remember_address": "on"})
    first = result(b, order_id(page) or "")
    check("member order earns points and saves the card", first.get("points_earned", 0) > 0 and first.get("card_saved"),
          str(first))

    # --- member checkout with the saved card -------------------------------------------------------------------
    b.request("/cart", {"product_id": PRODUCT, "quantity": "1"})
    s, _, page, _ = b.request("/cart")
    saved = re.search(r'name="saved_payment_id" value="([0-9a-f]+)" checked', page)
    check("saved card and address are offered at checkout",
          saved and "Visa ending in 0454" in page and 'value="1 Identity Way"' in page)
    check("the card number itself is not on the page", "4432801561520454" not in page.split("new-card-fields")[0])
    s, _, page, _ = b.request("/cart/checkout", {"email": EMAIL, **ADDRESS, "saved_payment_id": saved.group(1) if saved else ""})
    second = result(b, order_id(page) or "")
    check("order with the saved card completes and earns points",
          "Your order is complete!" in page and second.get("points_earned", 0) > 0, f"{s} {second}")
    check("balance adds up", second.get("balance") == 100 + guest["claimable_points"] + first.get("points_earned", 0)
          + second.get("points_earned", 0), str(second))

    # --- personalization and preferences -------------------------------------------------------------------------
    b.request("/setCurrency", {"currency_code": "EUR"})
    s, _, page, _ = b.request("/")
    check("home is personal: greeting, points, buy it again", "Hi, Eve" in page and "Bronze" in page and "Buy it again" in page)
    s, _, page, _ = b.request("/account")
    check("account shows card, address, orders, preferred currency",
          "Visa ending in 0454" in page and "1 Identity Way" in page and "Guest order" in page
          and re.search(r'<option value="EUR" selected', page) is not None)

    # --- sign out / sign in again --------------------------------------------------------------------------------
    s, _, _, _ = b.request("/auth/logout", {}, headers={"Origin": "https://evil.example"})
    check("sign-out from another site is refused", s == 403, str(s))
    s, url, page, _ = b.request("/auth/logout", {})
    check("sign-out ends at the shop, anonymous, with an empty cart",
          url.rstrip("/") == BASE and "id-account" not in page and "shop_currency" not in b.cookies(),
          "%s greeted=%s cookies=%s" % (url[:80], "id-account" in page, sorted(b.cookies())))
    s, url, page, _ = b.request("/account")
    check("the account page asks an anonymous shopper to sign in", not url.startswith(BASE), url[:80])
    s, url, page, _ = b.request(form_action(page), {"username": EMAIL, "password": PASSWORD})
    check("sign-in returns to the account, data intact", url == BASE + "/account" and "Visa ending in 0454" in page, url[:80])
    check("preferred currency is applied at sign-in", b.cookies().get("shop_currency") == "EUR", str(b.cookies()))

    # --- the member's controls -----------------------------------------------------------------------------------
    card_id = re.search(r"/account/payment-methods/([0-9a-f]+)/delete", page)
    s, _, page, _ = b.request(f"/account/payment-methods/{card_id.group(1)}/delete", {})
    check("a saved card can be removed", "The card was removed." in page and "Visa ending in 0454" not in page)
    s, url, page, _ = b.request("/account/delete", {})
    check("deleting shop data signs the member out", url.rstrip("/") == BASE and "id-account" not in page, url[:80])

    print(f"\n{'all checks passed' if not failed else str(failed) + ' check(s) failed'} ({EMAIL})")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
