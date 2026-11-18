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

# 2. Tahap Eksekusi (Image resmi Playwright Microsoft - sudah include Python & browser Firefox lengkap)
FROM mcr.microsoft.com/playwright/python:v1.49.0-noble

WORKDIR /app

# Salin requirements dan instal Python library (playwright-stealth)
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Salin hasil kompilasi Go binary dari tahap builder
COPY --from=builder /app/bot-presensi .

# Salin Python cookie refresher script
COPY cookie_refresher.py .

# Salin static files
COPY static/ ./static/

# Berikan hak akses
RUN chmod -R 777 /app

# Buka port 7860
EXPOSE 7860

CMD ["./bot-presensi"]
