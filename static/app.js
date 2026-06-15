// Global State
let state = {
    profiles: [],
    config: {
        wa_token: '',
        wa_group: '',
        check_interval: 5,
        start_hour: 7,
        end_hour: 21
    }
};

// UI Elements
const profilesList = document.getElementById('profiles-list');
const logTerminal = document.getElementById('log-terminal');
const botStatus = document.getElementById('bot-status');
const streamStatus = document.getElementById('stream-status');

// Init
document.addEventListener('DOMContentLoaded', () => {
    fetchConfig();
    fetchProfiles();
    initLogStream();

    // Scroll to bottom immediately if user unchecks Freeze Scroll
    document.getElementById('freeze-scroll').addEventListener('change', (e) => {
        if (!e.target.checked) {
            logTerminal.scrollTop = logTerminal.scrollHeight;
        }
    });
});

// Toast Notifications
function showToast(message, type = 'info') {
    const container = document.getElementById('toast-container');
    const toast = document.createElement('div');
    toast.className = `toast ${type}`;
    
    let icon = 'ℹ️';
    if (type === 'success') icon = '✅';
    if (type === 'error') icon = '❌';
    if (type === 'warning') icon = '⚠️';
    
    toast.innerHTML = `<span>${icon}</span><span>${message}</span>`;
    container.appendChild(toast);
    
    setTimeout(() => {
        toast.style.opacity = '0';
        toast.style.transform = 'translateX(50px)';
        setTimeout(() => toast.remove(), 300);
    }, 4000);
}

// Fetch Profiles
async function fetchProfiles() {
    try {
        const res = await fetch('/api/profiles');
        if (!res.ok) throw new Error('Gagal mengambil data profil');
        state.profiles = await res.json() || [];
        renderProfiles();
        updateStats();
    } catch (err) {
        showToast(err.message, 'error');
    }
}

// Fetch Config
async function fetchConfig() {
    try {
        const res = await fetch('/api/config');
        if (!res.ok) throw new Error('Gagal mengambil konfigurasi global');
        state.config = await res.json();
        renderConfig();
    } catch (err) {
        showToast(err.message, 'error');
    }
}

// Render Profiles Table
function renderProfiles() {
    if (state.profiles.length === 0) {
        profilesList.innerHTML = `
            <tr>
                <td colspan="6" class="table-empty">Belum ada akun terdaftar. Silakan tambah akun baru!</td>
            </tr>
        `;
        return;
    }

    profilesList.innerHTML = state.profiles.map(p => {
        const activeBadge = p.is_active 
            ? `<span class="badge badge-success">Aktif</span>` 
            : `<span class="badge badge-muted">Nonaktif</span>`;
            
        let statusBadge = `<span class="badge badge-muted">Belum Cek</span>`;
        if (p.last_run_status === 'success') {
            statusBadge = `<span class="badge badge-success">Sukses Presensi</span>`;
        } else if (p.last_run_status === 'failed') {
            statusBadge = `<span class="badge badge-danger" title="${p.last_run_msg || ''}">Gagal</span>`;
        } else if (p.last_run_status === 'no_link') {
            statusBadge = `<span class="badge badge-info">Tidak Ada Matkul Aktif</span>`;
        } else if (p.last_run_status === 'expired') {
            statusBadge = `<span class="badge badge-warning">Cookie Expired</span>`;
        }

        const lastCheck = p.last_run_time && p.last_run_time !== '0001-01-01T00:00:00Z'
            ? new Date(p.last_run_time).toLocaleString('id-ID')
            : '-';

        return `
            <tr>
                <td><span class="profile-nim">${p.nim}</span></td>
                <td><span class="profile-name">${escapeHTML(p.name)}</span></td>
                <td>
                    <div style="font-size:0.8rem;color:var(--text-secondary)">
                        ${p.wa_group ? escapeHTML(p.wa_group) : '<span style="color:var(--text-muted)">Gunakan Global</span>'}
                    </div>
                </td>
                <td>
                    <div class="status-check">
                        ${statusBadge}
                        ${p.last_run_msg ? `<span class="status-time" style="max-width:180px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis" title="${escapeHTML(p.last_run_msg)}">${escapeHTML(p.last_run_msg)}</span>` : ''}
                    </div>
                </td>
                <td>
                    <div class="status-check">
                        <span>${activeBadge}</span>
                        <span class="status-time">${lastCheck}</span>
                    </div>
                </td>
                <td style="text-align: right;">
                    <div class="action-cell">
                        <button class="btn btn-icon-sm" onclick="triggerRunNow('${p.nim}')" title="Jalankan Presensi Sekarang">
                            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="icon"><polygon points="5 3 19 12 5 21 5 3"></polygon></svg>
                        </button>
                        <button class="btn btn-icon-sm" onclick="openEditProfileModal('${p.nim}')" title="Edit Akun">
                            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="icon"><path d="M12 20h9"></path><path d="M16.5 3.5a2.121 2.121 0 0 1 3 3L7 19l-4 1 1-4L16.5 3.5z"></path></svg>
                        </button>
                        <button class="btn btn-icon-sm" style="color:var(--danger)" onclick="deleteProfile('${p.nim}')" title="Hapus Akun">
                            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="icon"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg>
                        </button>
                    </div>
                </td>
            </tr>
        `;
    }).join('');
}

// Render Global Config Form
function renderConfig() {
    document.getElementById('cfg-wa-token').value = state.config.wa_token || '';
    document.getElementById('cfg-wa-group').value = state.config.wa_group || '';
    document.getElementById('cfg-interval').value = state.config.check_interval || 5;
    document.getElementById('cfg-start-hour').value = state.config.start_hour !== undefined ? state.config.start_hour : 7;
    document.getElementById('cfg-end-hour').value = state.config.end_hour !== undefined ? state.config.end_hour : 21;
    
    updateStats();
}

// Update Stats Cards
function updateStats() {
    document.getElementById('stat-total-profiles').innerText = state.profiles.length;
    document.getElementById('stat-active-profiles').innerText = state.profiles.filter(p => p.is_active).length;
    document.getElementById('stat-check-interval').innerText = `${state.config.check_interval} Detik`;
    
    const pad = (num) => String(num).padStart(2, '0');
    document.getElementById('stat-active-hours').innerText = 
        `${pad(state.config.start_hour)}:00 - ${pad(state.config.end_hour)}:00`;

    // Bot Active status
    const now = new Date();
    // Gunakan WIB (UTC+7)
    const utc = now.getTime() + (now.getTimezoneOffset() * 60000);
    const wib = new Date(utc + (3600000 * 7));
    const hour = wib.getHours();
    
    botStatus.className = 'bot-status-badge';
    if (hour >= state.config.start_hour && hour < state.config.end_hour) {
        if (state.profiles.some(p => p.is_active)) {
            botStatus.classList.add('running');
            botStatus.querySelector('.status-text').innerText = 'Bot Aktif (Scheduler Jalan)';
        } else {
            botStatus.classList.add('idles');
            botStatus.querySelector('.status-text').innerText = 'Idle (Tidak ada akun aktif)';
        }
    } else {
        botStatus.classList.add('sleeping');
        botStatus.querySelector('.status-text').innerText = 'Bot Tidur (Di luar jam aktif)';
    }
}

// Save Global Config
async function saveGlobalConfig() {
    const config = {
        wa_token: document.getElementById('cfg-wa-token').value,
        wa_group: document.getElementById('cfg-wa-group').value,
        check_interval: parseInt(document.getElementById('cfg-interval').value) || 5,
        start_hour: parseInt(document.getElementById('cfg-start-hour').value) || 7,
        end_hour: parseInt(document.getElementById('cfg-end-hour').value) || 21
    };

    try {
        const res = await fetch('/api/config', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(config)
        });
        if (!res.ok) throw new Error('Gagal menyimpan konfigurasi');
        state.config = config;
        showToast('Konfigurasi global berhasil disimpan!', 'success');
        updateStats();
    } catch (err) {
        showToast(err.message, 'error');
    }
}

// Modals Handling
const modal = document.getElementById('profile-modal');

function openAddProfileModal() {
    document.getElementById('modal-title').innerText = 'Tambah Akun Baru';
    document.getElementById('prof-action').value = 'add';
    document.getElementById('prof-nim').value = '';
    document.getElementById('prof-nim').readOnly = false;
    document.getElementById('prof-name').value = '';
    document.getElementById('prof-cookies').value = '';
    document.getElementById('prof-wa-token').value = '';
    document.getElementById('prof-wa-group').value = '';
    document.getElementById('prof-active').checked = true;
    modal.classList.add('active');
}

function openEditProfileModal(nim) {
    const p = state.profiles.find(x => x.nim === nim);
    if (!p) return;

    document.getElementById('modal-title').innerText = `Edit Akun: ${p.nim}`;
    document.getElementById('prof-action').value = 'edit';
    document.getElementById('prof-nim').value = p.nim;
    document.getElementById('prof-nim').readOnly = true;
    document.getElementById('prof-name').value = p.name;
    document.getElementById('prof-cookies').value = p.cookies_json || '';
    document.getElementById('prof-wa-token').value = p.wa_token || '';
    document.getElementById('prof-wa-group').value = p.wa_group || '';
    document.getElementById('prof-active').checked = p.is_active;
    modal.classList.add('active');
}

function closeProfileModal() {
    modal.classList.remove('active');
}

// Submit Profile Add/Edit
async function handleProfileSubmit(e) {
    e.preventDefault();
    const action = document.getElementById('prof-action').value;
    const nim = document.getElementById('prof-nim').value;
    const cookiesText = document.getElementById('prof-cookies').value;

    // Validate cookies is valid JSON or String
    let validatedCookies = cookiesText.trim();
    if (validatedCookies.startsWith('[') || validatedCookies.startsWith('{')) {
        try {
            JSON.parse(validatedCookies);
        } catch(err) {
            showToast('Format JSON Cookies tidak valid! Silakan cek kembali.', 'error');
            return;
        }
    }

    const profile = {
        nim: nim,
        name: document.getElementById('prof-name').value,
        cookies_json: validatedCookies,
        wa_token: document.getElementById('prof-wa-token').value,
        wa_group: document.getElementById('prof-wa-group').value,
        is_active: document.getElementById('prof-active').checked
    };

    try {
        const res = await fetch('/api/profiles', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(profile)
        });
        if (!res.ok) {
            const errText = await res.text();
            throw new Error(errText || 'Gagal menyimpan profil');
        }
        
        showToast(action === 'add' ? 'Akun berhasil ditambahkan!' : 'Akun berhasil diperbarui!', 'success');
        closeProfileModal();
        fetchProfiles();
    } catch (err) {
        showToast(err.message, 'error');
    }
}

// Delete Profile
async function deleteProfile(nim) {
    if (!confirm(`Apakah Anda yakin ingin menghapus akun NIM ${nim}?`)) return;

    try {
        const res = await fetch(`/api/profiles?nim=${nim}`, {
            method: 'DELETE'
        });
        if (!res.ok) throw new Error('Gagal menghapus profil');
        showToast('Akun berhasil dihapus', 'success');
        fetchProfiles();
    } catch (err) {
        showToast(err.message, 'error');
    }
}

// Run bot for one account immediately
async function triggerRunNow(nim) {
    showToast(`Memulai pengecekan presensi untuk NIM ${nim}...`, 'info');
    try {
        const res = await fetch('/api/run-now', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ nim: nim })
        });
        if (!res.ok) throw new Error('Gagal memicu bot');
        showToast(`Bot presensi NIM ${nim} selesai dijalankan. Silakan cek Log!`, 'success');
        // Refresh profiles to see updated status
        setTimeout(fetchProfiles, 1500);
    } catch (err) {
        showToast(err.message, 'error');
    }
}

// Run bot for all accounts immediately
async function triggerRunAll() {
    showToast('Memulai pengecekan presensi untuk semua akun aktif...', 'info');
    try {
        const res = await fetch('/api/run-now', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ nim: '' }) // empty runs all
        });
        if (!res.ok) throw new Error('Gagal memicu bot');
        showToast('Bot presensi untuk semua akun selesai dijalankan!', 'success');
        setTimeout(fetchProfiles, 1500);
    } catch (err) {
        showToast(err.message, 'error');
    }
}

// Export Configuration
function exportConfig() {
    window.location.href = '/api/export';
    showToast('Mendownload file backup konfigurasi...', 'success');
}

// Import Configuration
async function importConfig(e) {
    const file = e.target.files[0];
    if (!file) return;

    const reader = new FileReader();
    reader.onload = async function(event) {
        try {
            const data = JSON.parse(event.target.result);
            const res = await fetch('/api/import', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(data)
            });
            if (!res.ok) throw new Error('Gagal mengimpor konfigurasi');
            showToast('Konfigurasi berhasil dipulihkan!', 'success');
            fetchProfiles();
            fetchConfig();
        } catch (err) {
            showToast(`Kesalahan Impor: ${err.message}`, 'error');
        }
    };
    reader.readAsText(file);
    e.target.value = ''; // Reset file input
}

// Stream Logs
function initLogStream() {
    const sse = new EventSource('/api/logs/stream');
    
    sse.onopen = () => {
        streamStatus.className = 'stream-badge active';
        streamStatus.innerText = 'Live Connected';
    };
    
    sse.onerror = () => {
        streamStatus.className = 'stream-badge';
        streamStatus.innerText = 'Disconnected';
    };
    
    sse.onmessage = (event) => {
        try {
            const logMsg = JSON.parse(event.data);
            appendLog(logMsg);
        } catch (err) {
            // Raw text log fallback
            appendLog({
                timestamp: new Date().toISOString(),
                level: 'info',
                message: event.data
            });
        }
    };
}

function appendLog(logMsg) {
    const logLine = document.createElement('div');
    logLine.className = `log-line ${logMsg.level || 'info'}`;
    
    const timeStr = logMsg.timestamp 
        ? new Date(logMsg.timestamp).toLocaleTimeString('id-ID')
        : new Date().toLocaleTimeString('id-ID');
        
    logLine.innerHTML = `<span class="timestamp">[${timeStr}]</span>${escapeHTML(logMsg.message)}`;
    logTerminal.appendChild(logLine);
    
    // Auto Scroll to Bottom (if not frozen)
    const isFrozen = document.getElementById('freeze-scroll')?.checked;
    if (!isFrozen) {
        logTerminal.scrollTop = logTerminal.scrollHeight;
    }
    
    // Limit logs inside DOM
    while (logTerminal.childElementCount > 300) {
        logTerminal.firstElementChild.remove();
    }
}

function clearLogs() {
    logTerminal.innerHTML = '<div class="log-line system">=== Log dibersihkan ===</div>';
}

// Helper Helpers
function escapeHTML(str) {
    if (!str) return '';
    return str.replace(/[&<>'"]/g, 
        tag => ({
            '&': '&amp;',
            '<': '&lt;',
            '>': '&gt;',
            "'": '&#39;',
            '"': '&quot;'
        }[tag] || tag)
    );
}
