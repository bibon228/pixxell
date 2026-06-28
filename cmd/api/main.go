package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
)

//go:embed static/*
var staticFiles embed.FS

// Upgrader "прокачивает" обычное HTTP-соединение до постоянного WebSocket-кабеля
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Разрешаем подключение с любых адресов (чтобы локально всё работало без проблем)
	},
}

// Секретный ключ для подписи токенов (в реальном мире он хранится в переменных окружения)
var jwtKey = []byte("my_super_secret_key_for_princess")

type LoginRequest struct {
	Username string `json:"username"`
}

// loginHandler генерирует защищенный токен по никнейму
func loginHandler(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil || req.Username == "" {
		http.Error(w, "Укажите никнейм", http.StatusBadRequest)
		return
	}

	// Создаем токен с данными (Claims)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"username": req.Username,
	})

	// Подписываем секретным ключом
	tokenString, err := token.SignedString(jwtKey)
	if err != nil {
		http.Error(w, "Ошибка генерации токена", http.StatusInternalServerError)
		return
	}

	// Отправляем токен обратно в браузер
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": tokenString})
}

// wsHandler вызывается каждый раз, когда браузер просит открыть WebSocket

func canPlacePixel(ctx context.Context, rdb *redis.Client, userID string) bool {
	key := fmt.Sprintf("cooldown:%s", userID)
	isAllowed, _ := rdb.SetNX(ctx, key, "locked", 10*time.Second).Result()
	return isAllowed
}
func setPixel(ctx context.Context, rdb *redis.Client, x int, y int, color byte) error {
	width := 100 // Наш холст пока 100х100
	offset := int64(y*width + x)
	return rdb.SetRange(ctx, "canvas", offset, string([]byte{color})).Err()
}

// --- НОВЫЙ СЕРДЦЕВИДНЫЙ ХЕНДЛЕР ---
func wsHandler(w http.ResponseWriter, r *http.Request) {
	// Читаем токен из ссылки: ws://localhost:8080/ws?token=...
	tokenString := r.URL.Query().Get("token")
	if tokenString == "" {
		http.Error(w, "Отсутствует токен", http.StatusUnauthorized)
		return
	}

	// Проверяем подлинность токена
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return jwtKey, nil
	})
	if err != nil || !token.Valid {
		http.Error(w, "Неверный токен", http.StatusUnauthorized)
		return
	}

	// Достаем никнейм из токена
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || claims["username"] == nil {
		http.Error(w, "Неверные данные в токене", http.StatusUnauthorized)
		return
	}
	username := claims["username"].(string)

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// 1. РЕГИСТРАЦИЯ: Привязываем кабель к КОНКРЕТНОМУ никнейму
	clients[conn] = username
	fmt.Printf("🎉 Игрок %s подключился. Всего онлайн: %d\n", username, len(clients))

	defer func() {
		delete(clients, conn) // Удаляем кабель из списка
		conn.Close()
		fmt.Printf("Игрок %s отключился. Онлайн: %d\n", username, len(clients))
	}()
	ctx := context.Background()
	// --- НОВЫЙ КОД: Отправляем весь холст при подключении ---
	// Запрашиваем сколько пикселей уже поставил этот юзер за всю историю
	myPixelsCount, _ := rdb.Get(ctx, "pixels:"+username).Int64()

	// Читаем всю строку холста (10000 байт) из Redis
	canvasStr, err := rdb.Get(ctx, "canvas").Result()
	if err == nil {
		// Превращаем строку в массив чисел, чтобы JS было удобно
		pixels := make([]int, len(canvasStr))
		for i := 0; i < len(canvasStr); i++ {
			pixels[i] = int(canvasStr[i])
		}
		// Отправляем специальное сообщение типа "init" с холстом и счетом
		conn.WriteJSON(map[string]interface{}{
			"type":         "init",
			"canvas":       pixels,
			"pixelsPlaced": myPixelsCount,
		})
	}
	// ---------------------------------------------------------
	// Бесконечный цикл прослушивания...
	for {
		var p Pixel

		// 2. ПРИЕМ: Магия Go - читаем JSON из браузера сразу в нашу структуру Pixel!
		err := conn.ReadJSON(&p)
		if err != nil {
			break // Ошибка чтения (игрок ушел), выходим из цикла, сработает defer
		}

		// ЗАЩИТА АВТОРИЗАЦИИ: Игнорируем UserID, который прислал браузер.
		// Берем надежный никнейм, который мы проверили при подключении!
		p.UserID = clients[conn]

		// 3. RATE LIMITER: Проверяем кулдаун
		if !canPlacePixel(ctx, rdb, p.UserID) {
			// Если рано, отправляем лично этому юзеру "пустой" пиксель как знак ошибки
			// (пока что это просто заглушка для фронтенда)
			conn.WriteJSON(Pixel{UserID: "system", Color: 255})
			continue // Прерываем текущий круг цикла, идем ждать следующий клик
		}
		// 4. БАЗА ДАННЫХ: Сохраняем цвет в Redis
		err = setPixel(ctx, rdb, p.X, p.Y, p.Color)
		if err != nil {
			fmt.Println("Ошибка сохранения в Redis:", err)
			continue
		}

		// Инкрементируем счетчик поставленных пикселей для игрока
		userPixels, _ := rdb.Incr(ctx, "pixels:"+p.UserID).Result()
		p.Pixels = userPixels

		// 5. БРОДКАСТ (Рассылка всем):
		fmt.Printf("Игрок %s поставил цвет %d на координатах %d:%d\n", p.UserID, p.Color, p.X, p.Y)

		// Пробегаемся по ВСЕМ открытым соединениям (всем вкладкам браузера)
		for client := range clients {
			// Отправляем этот же пиксель каждому из них
			client.WriteJSON(p)
		}
	}
}

type Pixel struct {
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Color  byte   `json:"color"`
	UserID string `json:"userID"`
	Pixels int64  `json:"pixels"` // Новое поле: сколько пикселей поставил юзер
}

// clients - тут мы храним все активные подключения (онлайн)
// Ключ - само подключение (кабель), значение - имя пользователя (никнейм)
var clients = make(map[*websocket.Conn]string)

// Глобальная переменная для базы
var rdb *redis.Client
var pgdb *sql.DB

func backupWorker() {
	for {
		// Ждем 1 минуту перед каждым сохранением
		time.Sleep(1 * time.Minute)

		ctx := context.Background()
		canvasStr, err := rdb.Get(ctx, "canvas").Result()
		if err != nil {
			fmt.Println("backupWorker: ошибка чтения холста из Redis (или он еще пуст):", err)
			continue
		}

		// Переводим строку обратно в сырые байты для сохранения
		_, err = pgdb.Exec("INSERT INTO canvas_history (canvas_data) VALUES ($1)", []byte(canvasStr))
		if err != nil {
			fmt.Println("backupWorker: ошибка записи в Postgres:", err)
		} else {
			fmt.Println("backupWorker: 💾 Холст успешно сохранен в базу Postgres!")
		}
	}
}

// Эта функция срабатывает один раз при запуске сервера
func restoreCanvasFromDB() {
	var canvasData []byte

	// SQL-запрос: "Дай мне 1 самую свежую запись из истории"
	err := pgdb.QueryRow("SELECT canvas_data FROM canvas_history ORDER BY id DESC LIMIT 1").Scan(&canvasData)
	if err != nil {
		if err == sql.ErrNoRows {
			fmt.Println("restoreCanvas: Postgres пуст (это нормально при самом первом запуске).")
		} else {
			fmt.Println("restoreCanvas: Ошибка чтения из БД:", err)
		}
		return
	}

	// Записываем найденные данные в Redis
	ctx := context.Background()
	err = rdb.Set(ctx, "canvas", string(canvasData), 0).Err()
	if err != nil {
		fmt.Println("restoreCanvas: Ошибка записи в Redis:", err)
	} else {
		fmt.Println("✅ Успех! Холст восстановлен из БД!")
	}
}

func main() {
	// 1. Настраиваем Redis через переменные окружения
	redisURL := os.Getenv("REDIS_URL")
	if redisURL != "" {
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			log.Fatal("Ошибка парсинга REDIS_URL:", err)
		}
		rdb = redis.NewClient(opt)
	} else {
		// Локальный дефолт
		rdb = redis.NewClient(&redis.Options{
			Addr:     "localhost:6379",
			Password: "",
			DB:       0,
		})
	}

	// 2. Настраиваем Postgres через переменные окружения (обычно DATABASE_URL)
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "postgres://pixel_user:pixel_pass@127.0.0.1:5433/pixel_db?sslmode=disable"
	}

	var err error
	pgdb, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("Ошибка подключения к Postgres:", err)
	}

	// Создаем новую таблицу с правильным типом данных BYTEA
	_, err = pgdb.Exec(`
		CREATE TABLE IF NOT EXISTS canvas_history (
			id SERIAL PRIMARY KEY,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			canvas_data BYTEA NOT NULL
		);
	`)
	if err != nil {
		log.Fatal("Ошибка создания таблицы:", err)
	}

	// Восстанавливаем картину ИЗ Postgres В Redis до того, как зайдут первые игроки!
	restoreCanvasFromDB()

	go backupWorker()

	// Раздаем статические файлы (встроенные прямо в бинарник!)
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", http.FileServer(http.FS(staticFS)))

	// Регистрируем роут логина
	http.HandleFunc("/login", loginHandler)
	// Регистрируем наш новый роут для вебсокетов
	http.HandleFunc("/ws", wsHandler)

	// 3. Порт тоже берем из окружения (Railway требует этого!)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	fmt.Printf("🚀 Сервер запущен на порту %s\n", port)
	err = http.ListenAndServe(":"+port, nil)
	if err != nil {
		log.Fatal("Ошибка запуска сервера:", err)
	}
}
