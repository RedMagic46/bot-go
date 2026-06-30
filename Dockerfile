# 1. Tahap Build (Kompilasi Go)
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Salin file modul dan install dependensi
COPY go.mod go.sum* ./
RUN go mod download
RUN go get -d -v ./...

# Salin source code
COPY . .

# Build aplikasi Go menjadi single binary bernama 'bot-presensi'
RUN CGO_ENABLED=0 GOOS=linux go build -o bot-presensi main.go

# 2. Tahap Eksekusi (Runtime dengan Python + Playwright Chromium untuk auto-refresh cookie)
FROM python:3.10-slim

# Set HOME variable to a directory accessible by user 1000
ENV HOME=/home/user
RUN mkdir -p /home/user/.cache && chmod -R 777 /home/user

# Instal dependensi sistem yang dibutuhkan oleh Playwright Chromium
RUN apt-get update && apt-get install -y \
    wget \
    gnupg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Salin requirements dan instal Python library (playwright, playwright-stealth)
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Instal browser Playwright (Chromium) beserta dependensi OS-nya
RUN python -m playwright install chromium
RUN python -m playwright install-deps chromium

# Salin hasil kompilasi Go binary dari tahap builder
COPY --from=builder /app/bot-presensi .

# Salin Python cookie refresher script
COPY cookie_refresher.py .

# Salin static files (sudah di-embed ke Go binary, tapi backup)
COPY static/ ./static/

# Berikan akses penuh untuk user non-root
RUN chmod -R 777 /app /home/user

# Bikin user dengan ID 1000 (standar wajib Hugging Face / Railway)
RUN adduser --disabled-password --uid 1000 user || true

# Gunakan user 1000 untuk menjalankan aplikasi
USER user

# Buka port 7860
EXPOSE 7860

CMD ["./bot-presensi"]
