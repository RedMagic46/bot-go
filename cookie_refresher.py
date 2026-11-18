#!/usr/bin/env python3
"""
Cookie Refresher - Headless browser login untuk UMM Infokhs
Dipanggil dari Go via subprocess.
"""

import argparse
import asyncio
import json
import sys


async def close_modal(page):
    """Dismiss bootstrap modals if they block input"""
    try:
        await page.keyboard.press("Escape")
        for selector in [
            'button:has-text("Close"):visible',
            ".modal.show button.close",
            ".modal.in button.close",
            "button.close",
        ]:
            btn = await page.query_selector(selector)
            if btn:
                await btn.click()
                await asyncio.sleep(0.5)

        await page.evaluate(
            """() => {
            if (typeof $ !== 'undefined') $('.modal').modal('hide');
            document.querySelectorAll('.modal.show, .modal.in').forEach(m => {
                m.classList.remove('show', 'in');
                m.style.display = 'none';
            });
            document.querySelectorAll('.modal-backdrop').forEach(b => b.remove());
            document.body.classList.remove('modal-open');
        }"""
        )
    except Exception:
        pass


async def fill_credentials(page, nim, password):
    """Fill login form automatically"""
    try:
        title = await page.title()
        log(f"Judul halaman: '{title}' (URL: {page.url})")

        if "Just a moment" in title or "Cloudflare" in title:
            log("⚠️ Halaman login tertahan verifikasi Cloudflare Turnstile.")
            return False

        # Coba tunggu form login muncul
        found_selector = None
        for sel in ['input[name="username"]', 'input[name="nim"]', '#username', 'input[type="text"]']:
            try:
                el = await page.wait_for_selector(sel, timeout=5000)
                if el and await el.is_visible():
                    found_selector = sel
                    break
            except Exception:
                pass

        if not found_selector:
            log(f"⚠️ Form input username tidak ditemukan. Judul halaman saat ini: '{await page.title()}'")
            return False

        await close_modal(page)

        if nim:
            await page.fill(found_selector, nim)
            await asyncio.sleep(0.5)
        if password:
            for p_sel in ['input[name="password"]', '#password', 'input[type="password"]']:
                p_el = await page.query_selector(p_sel)
                if p_el and await p_el.is_visible():
                    await p_el.fill(password)
                    break
            await asyncio.sleep(0.5)

        await close_modal(page)
        
        # Submit via Enter
        await page.keyboard.press("Enter")
        return True
    except Exception as e:
        log(f"Form login tidak diisi otomatis: {e}")
        return False


def log(msg):
    """Print log to stderr (stdout reserved for JSON result)"""
    print(msg, file=sys.stderr, flush=True)


async def run(nim, password):
    url_login = "https://infokhs.umm.ac.id/login"
    url_profil = "https://infokhs.umm.ac.id/profil"

    from playwright.async_api import async_playwright
    from playwright_stealth import Stealth

    pw_context = async_playwright()
    pw = await pw_context.__aenter__()

    # Hook stealth at playwright level
    stealth = Stealth()
    stealth.hook_playwright_context(pw)

    browser = None
    try:
        log("Meluncurkan Firefox headless...")
        browser = await pw.firefox.launch(headless=True)
        firefox_132_ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:132.0) Gecko/20100101 Firefox/132.0"
        context = await browser.new_context(user_agent=firefox_132_ua)
        page = await context.new_page()

        log(f"Membuka halaman login: {url_login}")
        await page.goto(url_login, timeout=25000)

        filled = await fill_credentials(page, nim, password)

        if not filled:
            current_title = await page.title()
            if "Just a moment" in current_title or "Cloudflare" in current_title:
                return {
                    "status": "error",
                    "error": "IP Cloud Azure terkena Challenge Cloudflare. Solusi: Salin cookies dari browser laptop ke menu Edit Profil di Dashboard."
                }

        log("Menunggu login sukses (max 2 menit)...")
        success = False
        for _ in range(120):
            await asyncio.sleep(1)
            if page.is_closed():
                break
            current_url = page.url
            if "profil" in current_url or "jadwal" in current_url:
                success = True
                break

        if not success:
            return {"status": "error", "error": "Login timeout. Pastikan NIM dan password benar atau perbarui cookie secara manual."}

        log("Login sukses!")
        await page.wait_for_load_state("domcontentloaded")
        await asyncio.sleep(2)

        # Navigate to profile page to get student info
        if "profil" not in page.url:
            await page.goto(url_profil, wait_until="domcontentloaded")
            await asyncio.sleep(1.5)

        # Extract student info
        info = await page.evaluate(
            r"""() => {
            let nim = "";
            let name = "";
            for (const row of document.querySelectorAll('tr')) {
                const cells = row.querySelectorAll('td, th');
                if (cells.length >= 2) {
                    const label = cells[0].textContent.trim().toLowerCase();
                    const val = cells[1].textContent.trim().replace(/^:\s*/, '');
                    if (label === 'nim' || label.includes('nomor induk')) {
                        nim = val;
                    } else if (label === 'nama' || (label.includes('nama') && !label.includes('dosen') && !label.includes('ayah') && !label.includes('ibu') && !label.includes('wali'))) {
                        name = val;
                    }
                }
            }
            return { nim, name };
        }"""
        )

        extracted_nim = info.get("nim") or nim
        student_name = info.get("name") or "Unknown"

        log(f"Akun: {student_name} ({extracted_nim})")

        # Extract cookies
        cookies = await context.cookies()
        user_agent = await page.evaluate("navigator.userAgent")

        formatted_cookies = []
        for c in cookies:
            formatted_cookies.append(
                {
                    "name": c.get("name", ""),
                    "value": c.get("value", ""),
                    "domain": c.get("domain", ""),
                    "path": c.get("path", "/"),
                    "expires": c.get("expires", -1),
                    "httpOnly": c.get("httpOnly", False),
                    "secure": c.get("secure", False),
                    "sameSite": c.get("sameSite", "Lax"),
                }
            )

        cookie_json_obj = {
            "user_agent": user_agent,
            "cookies": formatted_cookies,
        }

        return {
            "status": "success",
            "nim": extracted_nim,
            "name": student_name,
            "cookies_json": json.dumps(cookie_json_obj, indent=2),
            "user_agent": user_agent,
        }

    except Exception as e:
        log(f"Error di cookie_refresher: {e}")
        return {"status": "error", "error": str(e)}

    finally:
        if browser:
            try:
                await browser.close()
            except Exception:
                pass
        try:
            await pw_context.__aexit__(None, None, None)
        except Exception:
            pass


def main():
    parser = argparse.ArgumentParser(description="Refresh UMM cookies via headless browser")
    parser.add_argument("--nim", required=True, help="NIM Mahasiswa")
    parser.add_argument("--password", required=True, help="Password Mahasiswa (plain text)")
    args = parser.parse_args()

    result = asyncio.run(run(args.nim, args.password))
    # Output JSON to stdout
    print(json.dumps(result))


if __name__ == "__main__":
    main()
