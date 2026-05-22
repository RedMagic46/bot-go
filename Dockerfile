# 1. Tahap Build (Kompilasi)
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Salin file modul dan install dependensi
COPY go.mod go.sum* ./
RUN go mod download
RUN go get -d -v ./...

# Salin source code
COPY main.go ./

# Build aplikasi Go menjadi single binary bernama 'bot-presensi'
RUN CGO_ENABLED=0 GOOS=linux go build -o bot-presensi main.go

# 2. Tahap Eksekusi (Image akhir yang ultra ringan)
FROM alpine:latest

# Bikin user dengan ID 1000 (standar wajib Hugging Face)
RUN adduser -D -u 1000 user

WORKDIR /app

# Salin hasil kompilasi (binary) dari tahap builder
COPY --from=builder /app/bot-presensi .

# Ubah kepemilikan file ke user 1000
RUN chown -R user:user /app

# Gunakan user 1000 untuk menjalankan aplikasi
USER user

# Buka port 7860 untuk Hugging Face
EXPOSE 7860

CMD ["./bot-presensi"]
