#!/usr/bin/env python3
"""Pair a fresh, visible Rain browser session with a DueRadar instance.

Python 3.10+; install optional desktop dependencies with
  python -m pip install -r scripts/requirements-assist.txt
  python -m playwright install chromium
The instance URL and flow ID are public. Enter the one-use pairing code only
at the hidden prompt. Passwords and CAPTCHA interaction stay on Rain's page.
"""
import argparse
import asyncio
import datetime
import getpass
import ipaddress
import json
import math
import re
import ssl
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import warnings


RAIN_LOGIN = "https://www.yuketang.cn/web"
RAIN_IDENTITY = "https://www.yuketang.cn/api/v3/user/basic-info"
MAX_BODY = 64 * 1024
MAX_COOKIES = 256
MAX_HEADER = 32 * 1024
POLL_SECONDS = 2
FLOW_SECONDS = 300
COOKIE_NAME = re.compile(r"[!#$%&'*+\-.^_`|~0-9A-Za-z]+\Z")
OPAQUE_ID = re.compile(r"[A-Za-z0-9_-]{16,128}\Z")
CAPABILITY = re.compile(r"[A-Za-z0-9_-]{32,256}\Z")
TERMINAL_MESSAGES = {
    "failed": "登录验证失败，请在 DueRadar 重新发起绑定。",
    "expired": "本次绑定已过期，请在 DueRadar 重新发起。",
    "canceled": "本次绑定已取消。",
}


class AssistError(Exception):
    """A fixed public message, never an upstream body or credential."""


def instance_origin(value):
    """Normalize only a root origin; HTTP is restricted to explicit local/VPN IPs."""
    invalid = "实例地址须为 HTTPS 根地址，或本机／Tailscale 的 HTTP 根地址。"
    if not isinstance(value, str) or not value or any(ord(c) <= 32 or ord(c) == 127 for c in value) or "\\" in value:
        raise AssistError(invalid)
    try:
        parts = urllib.parse.urlsplit(value)
        host, port = parts.hostname, parts.port
    except ValueError:
        raise AssistError(invalid) from None
    if (parts.scheme not in ("https", "http") or not host or
            parts.username is not None or parts.password is not None or
            parts.path not in ("", "/") or parts.query or parts.fragment or
            "?" in value or "#" in value or "%" in host or
            port is not None and not 1 <= port <= 65535):
        raise AssistError(invalid)
    try:
        address = ipaddress.ip_address(host)
    except ValueError:
        address = None
        if not re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?", host):
            raise AssistError(invalid) from None
    if parts.scheme == "http":
        local = host == "localhost" or address is not None and address.is_loopback
        vpn = address is not None and (
            address.version == 4 and address in ipaddress.ip_network("100.64.0.0/10") or
            address.version == 6 and address in ipaddress.ip_network("fd7a:115c:a1e0::/48")
        )
        if not local and not vpn:
            raise AssistError(invalid)
    authority = "[" + host + "]" if ":" in host else host
    if port is not None:
        authority += ":" + str(port)
    return parts.scheme + "://" + authority


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def parse_json(data):
    def unique(pairs):
        value = {}
        for key, item in pairs:
            if key in value:
                raise ValueError("duplicate field")
            value[key] = item
        return value

    if len(data) > MAX_BODY:
        raise AssistError("服务响应超过大小限制，请重新发起绑定。")
    try:
        value = json.loads(data.decode("utf-8"), object_pairs_hook=unique,
                           parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
    except (ValueError, UnicodeError, RecursionError):
        raise AssistError("服务响应格式异常，请重新发起绑定。") from None
    if not isinstance(value, dict):
        raise AssistError("服务响应格式异常，请重新发起绑定。")
    return value


class InstanceClient:
    def __init__(self, instance, flow_id, opener=None):
        self.origin = instance_origin(instance)
        if not isinstance(flow_id, str) or OPAQUE_ID.fullmatch(flow_id) is None:
            raise AssistError("绑定编号格式无效，请使用 DueRadar 显示的编号。")
        self.path = self.origin + "/api/accounts/rain/assist/" + flow_id
        self.opener = opener or urllib.request.build_opener(
            urllib.request.ProxyHandler({}), NoRedirect(),
            urllib.request.HTTPSHandler(context=ssl.create_default_context()))

    def request(self, action, token, payload=None, timeout=10):
        if action not in ("connect", "helper", "session"):
            raise AssistError("绑定请求无效。")
        if not isinstance(token, str) or CAPABILITY.fullmatch(token) is None:
            raise AssistError("配对凭据格式无效，请重新输入。")
        if payload is not None and (not isinstance(payload, bytes) or len(payload) > MAX_BODY):
            raise AssistError("浏览器会话超过大小限制。")
        headers = {"Authorization": "Bearer " + token, "Accept": "application/json",
                   "User-Agent": "DueRadar-Assist/1"}
        if action == "session":
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(self.path + "/" + action, data=payload,
                                         headers=headers, method="GET" if action == "helper" else "POST")
        expected = 202 if action == "session" else 200
        try:
            with self.opener.open(request, timeout=max(0.1, min(10, timeout))) as response:
                if response.geturl() != request.full_url:
                    raise AssistError("实例地址发生跳转，请检查地址后重新绑定。")
                if response.status != expected:
                    raise AssistError("实例未接受绑定请求，请在 DueRadar 重新发起。")
                data = response.read(MAX_BODY + 1)
        except urllib.error.HTTPError as error:
            code = error.code
            error.close()
            if 300 <= code < 400:
                raise AssistError("实例地址发生跳转，请检查地址后重新绑定。") from None
            if code in (401, 403, 404, 410):
                raise AssistError("配对凭据已失效或绑定已取消，请重新发起。") from None
            if code == 409:
                raise AssistError("本次会话已提交，请查看 DueRadar 状态；请勿重复提交。") from None
            raise AssistError("实例未接受绑定请求，请在 DueRadar 重新发起。") from None
        except AssistError:
            raise
        except Exception:
            raise AssistError("连接 DueRadar 失败，请检查实例地址和网络后重新绑定。") from None
        return parse_json(data)


def expiry_timestamp(value):
    if not isinstance(value, str) or re.fullmatch(
            r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?(?:Z|[+-]\d\d:\d\d)", value) is None:
        raise AssistError("绑定有效期格式异常，请重新发起。")
    normalized = re.sub(r"\.(\d+)(?=Z|[+-]\d\d:\d\d$)",
                        lambda m: "." + m.group(1)[:6].ljust(6, "0"), value)
    try:
        return datetime.datetime.fromisoformat(normalized.replace("Z", "+00:00")).timestamp()
    except (ValueError, OverflowError, OSError):
        raise AssistError("绑定有效期格式异常，请重新发起。") from None


def browser_session(cookies, now=None):
    """Convert only Rain cookies observed in this newly owned browser context."""
    now = time.time() if now is None else now
    if not isinstance(cookies, list) or len(cookies) > 4096:
        raise AssistError("浏览器会话格式或数量异常。")
    rows, identities, header_size = [], set(), 0
    effective = False
    for cookie in cookies:
        if not isinstance(cookie, dict) or not isinstance(cookie.get("domain"), str):
            raise AssistError("浏览器会话格式异常。")
        domain = cookie["domain"].lower()
        if domain not in ("www.yuketang.cn", ".www.yuketang.cn", ".yuketang.cn", "yuketang.cn"):
            continue
        name, value, path = cookie.get("name"), cookie.get("value"), cookie.get("path")
        if (not isinstance(name, str) or COOKIE_NAME.fullmatch(name) is None or
                not isinstance(value, str) or any(ord(c) < 32 or ord(c) >= 127 or c in '\\";' for c in value) or
                not isinstance(path, str) or not path.startswith("/") or
                any(ord(c) < 32 or ord(c) >= 127 or c == ";" for c in path) or
                type(cookie.get("secure")) is not bool or type(cookie.get("httpOnly")) is not bool or
                cookie.get("partitionKey")):
            raise AssistError("浏览器会话包含不支持的 Cookie 格式。")
        if len(name) + len(value) + len(path) > MAX_HEADER:
            raise AssistError("浏览器会话超过大小限制。")
        expires = cookie.get("expires")
        if (type(expires) not in (int, float) or expires > 253402300799 or
                not math.isfinite(expires) or expires < 0 and expires != -1):
            raise AssistError("浏览器 Cookie 有效期格式异常。")
        if expires != -1 and expires <= now:
            continue
        identity = (domain.lstrip("."), path, name)
        if identity in identities:
            raise AssistError("浏览器会话包含重复 Cookie。")
        identities.add(identity)
        browser_same_site = cookie.get("sameSite")
        if browser_same_site is not None and not isinstance(browser_same_site, str):
            raise AssistError("浏览器 Cookie SameSite 格式异常。")
        same_site = {None: 1, "Lax": 2, "Strict": 3, "None": 4}.get(browser_same_site)
        if same_site is None:
            raise AssistError("浏览器 Cookie SameSite 格式异常。")
        row = {"name": name, "value": value, "domain": domain, "path": path,
               "secure": cookie["secure"], "http_only": cookie["httpOnly"], "same_site": same_site}
        if expires != -1:
            try:
                row["expires"] = datetime.datetime.fromtimestamp(expires, datetime.timezone.utc).isoformat().replace("+00:00", "Z")
            except (ValueError, OverflowError, OSError):
                raise AssistError("浏览器 Cookie 有效期超出范围。") from None
        rows.append(row)
        header_size += len(name) + len(value) + 5
        if len(rows) > MAX_COOKIES or header_size > MAX_HEADER:
            raise AssistError("浏览器会话超过大小或数量限制。")
        identity_path = urllib.parse.urlsplit(RAIN_IDENTITY).path
        if identity_path == path or identity_path.startswith(path if path.endswith("/") else path + "/"):
            effective = True
    if not rows or not effective:
        raise AssistError("浏览器尚未建立可复用的雨课堂会话。")
    data = json.dumps({"version": 1, "cookies": rows}, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    if len(data) > MAX_BODY:
        raise AssistError("浏览器会话超过大小限制。")
    return data


async def bounded_thread(call, timeout):
    """A stalled OS resolver cannot hold interpreter shutdown's executor join."""
    loop = asyncio.get_running_loop()
    future = loop.create_future()

    def finish(result=None, error=None):
        if not future.done():
            if error is None:
                future.set_result(result)
            else:
                future.set_exception(error)

    def work():
        try:
            result, error = call(), None
        except Exception as caught:
            result, error = None, caught
        try:
            loop.call_soon_threadsafe(finish, result, error)
        except RuntimeError:
            pass  # The owner already exited; late responses are never replayed.

    threading.Thread(target=work, daemon=True).start()
    return await asyncio.wait_for(future, timeout)


async def identity_confirmed(context, timeout):
    response = None
    try:
        response = await context.request.get(RAIN_IDENTITY, timeout=max(1, int(timeout * 1000)),
                                             max_redirects=0, fail_on_status_code=False)
        if response.status != 200:
            return False
        value = parse_json(await response.body())
        data = value.get("data")
        return (type(value.get("code")) is int and value["code"] == 0 and
                isinstance(data, dict) and type(data.get("id")) is int and data["id"] > 0)
    except asyncio.CancelledError:
        raise
    except Exception:
        return False  # Login/challenge or transient read failure; never a login POST.
    finally:
        if response is not None:
            try:
                await asyncio.wait_for(response.dispose(), 2)
            except (Exception, asyncio.CancelledError):
                pass


async def close_owned(context, browser, driver, manager):
    for owner, method in ((context, "close"), (browser, "close"), (driver, "stop")):
        if owner is not None:
            try:
                await asyncio.wait_for(getattr(owner, method)(), 5)
            except (Exception, asyncio.CancelledError):
                pass
    if driver is None and manager is not None:
        try:
            await asyncio.wait_for(manager.__aexit__(None, None, None), 5)
        except (Exception, asyncio.CancelledError):
            pass


async def assist(client, pairing_code, channel=None, playwright_factory=None,
                 monotonic=time.monotonic, wall_time=time.time, sleep=asyncio.sleep, announce=print):
    if channel not in (None, "chrome", "msedge"):
        raise AssistError("浏览器渠道须为 chrome 或 msedge。")
    manager = driver = browser = context = None
    upload_token = None
    try:
        if playwright_factory is None:
            try:
                from playwright.async_api import async_playwright
            except ImportError:
                raise AssistError("请先安装 scripts/requirements-assist.txt，并运行 python -m playwright install chromium。") from None
            playwright_factory = async_playwright
        connected = await bounded_thread(lambda: client.request("connect", pairing_code), 12)
        pairing_code = None
        upload_token = connected.get("upload_token")
        if connected.get("state") != "awaiting_user" or not isinstance(upload_token, str) or CAPABILITY.fullmatch(upload_token) is None:
            raise AssistError("配对响应格式异常，请在 DueRadar 重新发起。")
        expires_at = expiry_timestamp(connected.get("expires_at"))
        remaining = min(FLOW_SECONDS, expires_at - wall_time())
        if remaining <= 0:
            raise AssistError(TERMINAL_MESSAGES["expired"])
        deadline = monotonic() + remaining

        def budget(maximum=10):
            left = deadline - monotonic()
            if left <= 0:
                raise AssistError(TERMINAL_MESSAGES["expired"])
            return min(maximum, left)

        async def remote(action, payload=None):
            limit = budget()
            return await bounded_thread(lambda: client.request(action, upload_token, payload, timeout=limit), limit)

        async def state():
            nonlocal deadline
            value = await remote("helper")
            new_expiry = expiry_timestamp(value.get("expires_at"))
            deadline = min(deadline, monotonic() + max(0, new_expiry - wall_time()))
            budget()
            status = value.get("state")
            if status in TERMINAL_MESSAGES:
                raise AssistError(TERMINAL_MESSAGES[status])
            if status not in ("awaiting_user", "verifying", "logged_in"):
                raise AssistError("绑定状态异常，请在 DueRadar 重新发起。")
            return status

        if await state() != "awaiting_user":
            raise AssistError("本次绑定已提交，请在 DueRadar 查看状态。")
        manager = playwright_factory()
        driver = await asyncio.wait_for(manager.start(), budget(20))
        options = {"headless": False}
        if channel is not None:
            options["channel"] = channel
        browser = await asyncio.wait_for(driver.chromium.launch(**options), budget(30))
        context = await asyncio.wait_for(browser.new_context(accept_downloads=False), budget())
        page = await asyncio.wait_for(context.new_page(), budget())
        await asyncio.wait_for(page.goto(RAIN_LOGIN, wait_until="domcontentloaded", timeout=int(budget(20) * 1000)), budget(20))
        announce("请在新打开的雨课堂官方页面完成账号密码登录与验证码；完成后会自动交给 DueRadar 验证。")
        uploaded = False
        while True:
            if page.is_closed() or not browser.is_connected():
                raise AssistError("登录窗口已关闭，请在 DueRadar 结束本次绑定。")
            current = await state()
            if current == "logged_in":
                if not uploaded:
                    raise AssistError("绑定状态异常，请在 DueRadar 查看状态。")
                announce("雨课堂登录已确认，DueRadar 将独立同步任务；同步结果请查看网页。")
                return "logged_in"
            if not uploaded:
                if current != "awaiting_user":
                    raise AssistError("本次绑定已提交，请在 DueRadar 查看状态。")
                ready = await asyncio.wait_for(identity_confirmed(context, budget(5)), budget(7))
                if ready:
                    cookies = await asyncio.wait_for(context.cookies(), budget())
                    payload = browser_session(cookies, wall_time())
                    cookies = None
                    if await state() != "awaiting_user":
                        raise AssistError("本次绑定状态已改变，请查看 DueRadar。")
                    uploaded = True  # Set before I/O: an uncertain POST must not be retried.
                    submitted = await remote("session", payload)
                    payload = None
                    if submitted.get("state") != "verifying":
                        raise AssistError("会话提交结果异常，请在 DueRadar 查看状态。")
            await sleep(min(POLL_SECONDS, budget(POLL_SECONDS)))
    except AssistError:
        raise
    except asyncio.CancelledError:
        raise
    except asyncio.TimeoutError:
        raise AssistError("连接超时或绑定已过期，请在 DueRadar 查看状态后重新发起。") from None
    except Exception:
        raise AssistError("辅助登录未完成，请关闭本次绑定后重试。") from None
    finally:
        pairing_code = upload_token = None
        await close_owned(context, browser, driver, manager)


class PublicArgumentParser(argparse.ArgumentParser):
    def error(self, message):
        # argparse's normal error includes raw argv, possibly a misplaced secret.
        self.print_usage(sys.stderr)
        self.exit(2, "参数格式无效；配对码仅在隐藏提示中输入。\n")


def main(argv=None):
    parser = PublicArgumentParser(description="DueRadar 雨课堂账号密码辅助登录（密码及验证码仅在官方页面输入）")
    parser.add_argument("--instance", help="DueRadar 根地址；不填写则提示输入")
    parser.add_argument("--flow", help="网页显示的绑定编号；不含配对码")
    parser.add_argument("--browser-channel", choices=("chrome", "msedge"), help="使用已安装的浏览器；默认使用 Playwright Chromium")
    args = parser.parse_args(argv)
    code = None
    try:
        if sys.version_info < (3, 10):
            raise AssistError("桌面辅助登录需要 Python 3.10 或更新版本。")
        origin = instance_origin(args.instance or input("DueRadar 实例地址：").strip())
        flow = args.flow or input("绑定编号：").strip()
        client = InstanceClient(origin, flow)
        with warnings.catch_warnings():
            warnings.simplefilter("error", getpass.GetPassWarning)
            try:
                code = getpass.getpass("一次性配对码（输入不显示）：").strip()
            except getpass.GetPassWarning:
                raise AssistError("当前终端不支持隐藏输入，请在交互终端运行辅助程序。") from None
        asyncio.run(assist(client, code, args.browser_channel))
        return 0
    except (EOFError, KeyboardInterrupt):
        print("辅助登录已退出；如需结束绑定，请在 DueRadar 关闭本次绑定。", file=sys.stderr)
        return 130
    except AssistError as error:
        print(str(error), file=sys.stderr)
        return 1
    except Exception:
        print("辅助登录未完成，请在 DueRadar 查看状态。", file=sys.stderr)
        return 1
    finally:
        code = None


if __name__ == "__main__":
    sys.exit(main())
