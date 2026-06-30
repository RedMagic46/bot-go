#!/usr/bin/env python3
"""
Cookie Refresher - Headless browser login untuk UMM Infokhs
Dipanggil dari Go via subprocess.

Usage:
  python cookie_refresher.py --nim 202410370110027 --password SECRET

Output: JSON ke stdout dengan format:
  {"status": "success", "nim": "...", "name": "...", "cookies_json": "...", "user_agent": "..."}
  atau
  {"status": "error", "error": "..."}
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

        # Click login button
        login_btn = None
        try:
            login_btn = await page.wait_for_selector("button.submit", timeout=3000)
        except Exception:
            pass
        if not login_btn:
            try:
                login_btn = await page.wait_for_selector(
                    'button[type="submit"]', timeout=2000
                )
            except Exception:
                pass

        if login_btn:
            await login_btn.click()
        else:
            await page.keyboard.press("Enter")

    except Exception as e:
        log(f"Form login tidak diisi otomatis: {e}")


def log(msg):
    """Print log to stderr (stdout reserved for JSON result)"""
    print(msg, file=sys.stderr, flush=True)


async def run(nim, password):
    url_login = "https://infokhs.umm.ac.id/login"
    url_profil = "https://infokhs.umm.ac.id/profil"

    browser = None
    context = None
    page = None
    use_camoufox = False

    # Try Camoufox first (better Cloudflare bypass)
    try:
        from camoufox.async_api import AsyncCamoufox

        use_camoufox = True
        log("Menggunakan Camoufox (headless) untuk bypass Cloudflare...")
    except ImportError:
        log("Camoufox tidak tersedia, menggunakan Playwright standar...")

    if use_camoufox:
        try:
            browser = await AsyncCamoufox(
                headless=True, humanize=True
            ).__aenter__()
            page = await browser.new_page()
            context = page.context
        except Exception as e:
            log(f"Camoufox gagal: {e}. Fallback ke Playwright...")
            use_camoufox = False

    if not use_camoufox:
        from playwright.async_api import async_playwright

        pw = await async_playwright().__aenter__()
        try:
            browser = await pw.firefox.launch(headless=True)
        except Exception:
            browser = await pw.chromium.launch(headless=True)
        context = await browser.new_context()
        page = await context.new_page()

    try:
        log(f"Membuka halaman login: {url_login}")
        await page.goto(url_login)

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
