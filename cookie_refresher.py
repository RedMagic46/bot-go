#!/usr/bin/env python3
"""
Cookie Refresher - Headless browser login untuk UMM Infokhs
Dipanggil dari Go via subprocess.

Menggunakan standard Playwright Chromium + Stealth untuk bypass Cloudflare.
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
        await page.wait_for_selector('input[name="username"]', timeout=15000)
        await close_modal(page)

        if nim:
            await page.fill('input[name="username"]', nim)
            await asyncio.sleep(0.5)
        if password:
            await page.fill('input[name="password"]', password)
            await asyncio.sleep(0.5)

        await close_modal(page)
        
        # Submit via Enter
        await page.keyboard.press("Enter")
    except Exception as e:
        log(f"Form login tidak diisi otomatis: {e}")


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
        # Gunakan User-Agent Firefox 132 agar sesuai dengan profile tls-client di Go (menghindari block Cloudflare 403)
        firefox_132_ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:132.0) Gecko/20100101 Firefox/132.0"
        context = await browser.new_context(user_agent=firefox_132_ua)
        page = await context.new_page()

        log(f"Membuka halaman login: {url_login}")
        await page.goto(url_login, timeout=20000)

        await fill_credentials(page, nim, password)

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
            return {"status": "error", "error": "Login timeout - gagal login dalam 2 menit. Pastikan NIM dan password benar."}

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

        cookie_data = {"user_agent": user_agent, "cookies": cookies}

        return {
            "status": "success",
            "nim": extracted_nim,
            "name": student_name,
            "cookies_json": json.dumps(cookie_data, indent=2),
            "user_agent": user_agent,
        }

    finally:
        if browser:
            await browser.close()
        if pw_context:
            await pw_context.__aexit__(None, None, None)


def main():
    parser = argparse.ArgumentParser(description="UMM Cookie Refresher")
    parser.add_argument("--nim", required=True, help="NIM mahasiswa")
    parser.add_argument("--password", required=True, help="Password login")
    args = parser.parse_args()

    try:
        result = asyncio.run(run(args.nim, args.password))
    except Exception as e:
        result = {"status": "error", "error": str(e)}

    # Output JSON to stdout (Go reads this)
    print(json.dumps(result), flush=True)
    sys.exit(0 if result.get("status") == "success" else 1)


if __name__ == "__main__":
    main()
