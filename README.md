# Pixel Battle 🎨

Real-Time Multiplayer Canvas (inspired by r/place).
Built with **Go**, **WebSockets**, **Redis**, and **PostgreSQL**.

## Architecture
* **Frontend**: HTML5 Canvas, Vanilla JS, JWT Authentication via LocalStorage.
* **Backend**: Golang `net/http` + `gorilla/websocket`.
* **Caching**: Redis (for sub-millisecond pixel drawing and Rate Limiting).
* **Database**: PostgreSQL (for persistent background backups).

## Features
* 🚀 **Real-Time**: Sub-millisecond latency broadcasting.
* 🛡️ **Rate Limiting**: 1 pixel per 10 seconds per user.
* 🔒 **Security**: JWT tokens for user authentication (preventing username spoofing).
* 💾 **Persistence**: Automatic background worker saves the canvas from Redis to Postgres every minute. At startup, the server restores the canvas from Postgres into Redis.

## Running Locally
1. Start the databases:
```bash
docker-compose up -d
```
2. Start the API server:
```bash
cd cmd/api
go run main.go
```
3. Open your browser at `http://localhost:8080`
