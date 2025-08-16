package main

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID       int    `json:"id"`
	Email    string `json:"email"`
	Password string `json:"-"`
}

type Project struct {
	ID        string `json:"id"`
	UserID    int    `json:"user_id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Subdomain string `json:"subdomain"`
	CreatedAt int64  `json:"created_at"`
	BuildLog  string `json:"build_log,omitempty"`
}

type Claims struct {
	UserID int `json:"user_id"`
	jwt.RegisteredClaims
}

var (
	db           *sql.DB
	jwtSecret    = []byte("grape-ai-secret-key-change-in-production")
	uploadsDir   = "uploads"
	projectsDir  = "projects"
	deployDir    = "deploy"
	pythonWorker = "../builder/worker.py"
)

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", "grape.db")
	if err != nil {
		log.Fatal(err)
	}

	// Create users table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT UNIQUE NOT NULL,
			password TEXT NOT NULL,
			created_at INTEGER DEFAULT (strftime('%s', 'now'))
		)
	`)
	if err != nil {
		log.Fatal(err)
	}

	// Create projects table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			status TEXT DEFAULT 'queued',
			subdomain TEXT NOT NULL,
			build_log TEXT DEFAULT '',
			created_at INTEGER DEFAULT (strftime('%s', 'now')),
			FOREIGN KEY (user_id) REFERENCES users (id)
		)
	`)
	if err != nil {
		log.Fatal(err)
	}
}

func ensureDirs() {
	for _, dir := range []string{uploadsDir, projectsDir, deployDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatal(err)
		}
	}
}

func generateID() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func hashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), 14)
	return string(bytes), err
}

func checkPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

func generateToken(userID int) (string, error) {
	claims := &Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

func validateToken(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		return jwtSecret, nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	return claims, nil
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		
		next.ServeHTTP(w, r)
	})
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Missing authorization header", http.StatusUnauthorized)
			return
		}

		tokenString := strings.TrimPrefix(authHeader, "Bearer ")
		claims, err := validateToken(tokenString)
		if err != nil {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		r = r.WithContext(context.WithValue(r.Context(), "userID", claims.UserID))
		next(w, r)
	}
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Email == "" || req.Password == "" {
		http.Error(w, "Email and password required", http.StatusBadRequest)
		return
	}

	hashedPassword, err := hashPassword(req.Password)
	if err != nil {
		http.Error(w, "Error hashing password", http.StatusInternalServerError)
		return
	}

	result, err := db.Exec("INSERT INTO users (email, password) VALUES (?, ?)", req.Email, hashedPassword)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			http.Error(w, "Email already exists", http.StatusConflict)
			return
		}
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	userID, _ := result.LastInsertId()
	token, err := generateToken(int(userID))
	if err != nil {
		http.Error(w, "Error generating token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token": token,
		"user":  map[string]interface{}{"id": userID, "email": req.Email},
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	var user User
	err := db.QueryRow("SELECT id, email, password FROM users WHERE email = ?", req.Email).
		Scan(&user.ID, &user.Email, &user.Password)
	if err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	if !checkPassword(req.Password, user.Password) {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := generateToken(user.ID)
	if err != nil {
		http.Error(w, "Error generating token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token": token,
		"user":  map[string]interface{}{"id": user.ID, "email": user.Email},
	})
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(int)
	
	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100MB max
		http.Error(w, "File too large", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	if name == "" {
		name = "project"
	}

	file, header, err := r.FormFile("project")
	if err != nil {
		http.Error(w, "Missing project file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if !strings.HasSuffix(header.Filename, ".zip") {
		http.Error(w, "Only .zip files allowed", http.StatusBadRequest)
		return
	}

	projectID := generateID()
	uploadPath := filepath.Join(uploadsDir, projectID+".zip")
	
	out, err := os.Create(uploadPath)
	if err != nil {
		http.Error(w, "Cannot save upload", http.StatusInternalServerError)
		return
	}
	defer out.Close()

	if _, err := io.Copy(out, file); err != nil {
		http.Error(w, "Cannot write upload", http.StatusInternalServerError)
		return
	}

	// Extract project
	projectPath := filepath.Join(projectsDir, projectID)
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		http.Error(w, "Cannot create project directory", http.StatusInternalServerError)
		return
	}

	if err := unzipFile(uploadPath, projectPath); err != nil {
		http.Error(w, "Cannot extract zip: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Save project to database
	subdomain := fmt.Sprintf("%s.grape.ai", projectID)
	_, err = db.Exec(`
		INSERT INTO projects (id, user_id, name, status, subdomain, created_at) 
		VALUES (?, ?, ?, 'queued', ?, ?)
	`, projectID, userID, name, subdomain, time.Now().Unix())
	
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	// Start build process
	go runBuild(projectID, projectPath)

	project := Project{
		ID:        projectID,
		UserID:    userID,
		Name:      name,
		Status:    "queued",
		Subdomain: subdomain,
		CreatedAt: time.Now().Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(project)
}

func handleProjects(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(int)
	
	rows, err := db.Query(`
		SELECT id, name, status, subdomain, created_at, build_log 
		FROM projects WHERE user_id = ? ORDER BY created_at DESC
	`, userID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var p Project
		err := rows.Scan(&p.ID, &p.Name, &p.Status, &p.Subdomain, &p.CreatedAt, &p.BuildLog)
		if err != nil {
			continue
		}
		p.UserID = userID
		projects = append(projects, p)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(projects)
}

func handleProjectStatus(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	projectID := vars["id"]
	userID := r.Context().Value("userID").(int)

	var project Project
	err := db.QueryRow(`
		SELECT id, name, status, subdomain, created_at, build_log 
		FROM projects WHERE id = ? AND user_id = ?
	`, projectID, userID).Scan(&project.ID, &project.Name, &project.Status, &project.Subdomain, &project.CreatedAt, &project.BuildLog)
	
	if err != nil {
		http.Error(w, "Project not found", http.StatusNotFound)
		return
	}

	project.UserID = userID
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(project)
}

func unzipFile(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(dest, f.Name)
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid file path: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, 0755)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			return err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()

		if err != nil {
			return err
		}
	}
	return nil
}

func runBuild(projectID, projectPath string) {
	// Update status to building
	db.Exec("UPDATE projects SET status = 'building' WHERE id = ?", projectID)

	deployPath := filepath.Join(deployDir, projectID)
	os.MkdirAll(deployPath, 0755)

	// Call Python worker
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pythonExec := "python3"
	if runtime.GOOS == "windows" {
		pythonExec = "python"
	}

	cmd := exec.CommandContext(ctx, pythonExec, pythonWorker, projectPath, deployPath)
	output, err := cmd.CombinedOutput()
	
	buildLog := string(output)
	status := "live"
	if err != nil {
		status = "failed"
		buildLog += fmt.Sprintf("\nError: %v", err)
	}

	// Update project status and build log
	db.Exec("UPDATE projects SET status = ?, build_log = ? WHERE id = ?", status, buildLog, projectID)
}

func main() {
	initDB()
	ensureDirs()

	r := mux.NewRouter()
	
	// Auth routes
	r.HandleFunc("/api/register", handleRegister).Methods("POST")
	r.HandleFunc("/api/login", handleLogin).Methods("POST")
	
	// Protected routes
	r.HandleFunc("/api/upload", authMiddleware(handleUpload)).Methods("POST")
	r.HandleFunc("/api/projects", authMiddleware(handleProjects)).Methods("GET")
	r.HandleFunc("/api/projects/{id}", authMiddleware(handleProjectStatus)).Methods("GET")

	// Serve static files from deploy directory
	r.PathPrefix("/deploy/").Handler(http.StripPrefix("/deploy/", http.FileServer(http.Dir(deployDir))))

	fmt.Println("🍇 Grape.ai API running on :8080")
	log.Fatal(http.ListenAndServe(":8080", corsMiddleware(r)))
}