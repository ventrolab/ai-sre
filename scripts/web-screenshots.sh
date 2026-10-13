#!/usr/bin/env bash
# Screenshots of web console pages as the local admin, for visual checks:
#   scripts/web-screenshots.sh /tmp/shots / /incidents /admin/users
# Needs google-chrome and node 22. Prints the saved file names.
. "$(dirname "$0")/lib.sh"
out="$1"; shift
WEB_URL="${WEB_URL:-http://localhost:8088}"
mkdir -p "$out"
cookie=$(WEB_URL="$WEB_URL" python3 - <<'PY'
import base64, json, os, re, subprocess, urllib.parse, urllib.request, http.cookiejar
secret = json.loads(subprocess.check_output(["kubectl", "-n", os.environ.get("NS", "ai-sre"), "get", "secret",
                                             os.environ.get("RELEASE", "ai-sre") + "-web", "-o", "json"]))
password = base64.b64decode(secret["data"]["WEB_ADMIN_PASSWORD"]).decode()
web = os.environ["WEB_URL"]
jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
token = re.search(r'name="csrf" value="([^"]+)"', opener.open(web + "/login").read().decode()).group(1)
data = urllib.parse.urlencode({"username": "admin", "password": password, "next": "/", "csrf": token}).encode()
opener.open(web + "/login", data)
print(next(c.value for c in jar if c.name == "aisre_session"))
PY
)
WEB_URL="$WEB_URL" node "$(dirname "$0")/web-screenshots.mjs" "$cookie" "$out" "$@"
