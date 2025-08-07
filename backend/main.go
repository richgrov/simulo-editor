package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/nedpals/supabase-go"
)

type Server struct {
	supabaseCli  *supabase.Client
	db           *DatabaseClient
	s3Client     *S3Client
	groqClient   *GroqClient
	compileQueue *JobQueue
	upgrader     websocket.Upgrader
}

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Println(".env not loaded")
	}

	supabaseCli := supabase.CreateClient(os.Getenv("SUPABASE_URL"), os.Getenv("SUPABASE_API_KEY"))

	// Initialize clients
	db, err := NewDatabaseClient(
		os.Getenv("POSTGRES_URL"),
		os.Getenv("SUPABASE_URL"),
		os.Getenv("SUPABASE_API_KEY"),
	)
	if err != nil {
		log.Fatal("Failed to initialize database:", err)
	}

	s3Client, err := NewS3Client(
		os.Getenv("S3_ENDPOINT"),
		os.Getenv("S3_ACCESS_KEY_ID"),
		os.Getenv("S3_SECRET_ACCESS_KEY"),
		os.Getenv("S3_BUCKET"),
	)
	if err != nil {
		log.Fatal("failed to initialize S3 client: ", err)
	}

	groqClient := NewGroqClient(os.Getenv("GROQ_API_KEY"))
	compileQueue := NewJobQueue()

	cors := os.Getenv("CORS")

	server := &Server{
		supabaseCli:  supabaseCli,
		db:           db,
		s3Client:     s3Client,
		groqClient:   groqClient,
		compileQueue: compileQueue,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				return origin == "" || origin == cors
			},
		},
	}

	// Setup routes
	http.HandleFunc("/project/", server.handleProjectAgent)
	http.HandleFunc("/projects", server.handleProjects)
	http.HandleFunc("/projects/create", server.createProject)
	http.HandleFunc("/projects/rename", server.renameProject)
	http.HandleFunc("/", server.handleWebSocket)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	fmt.Printf("Server running on port %s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func (s *Server) setCORSHeaders(w http.ResponseWriter) {
	cors := os.Getenv("CORS")
	if cors == "" {
		cors = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", cors)
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT")
}

func (s *Server) handleProjectAgent(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w)

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract project ID from URL
	projectID := s.parseProjectID(r.URL.Path)
	if projectID == "" {
		http.Error(w, "Project not found", http.StatusNotFound)
		return
	}

	// Parse form data
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	prompt := r.FormValue("prompt")
	if len(prompt) < 1 || len(prompt) > 1000 {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// Authorize user
	_, err := s.authorize(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Get project data
	projectData, err := s.db.GetProjectWithLatestDeployment(projectID)
	if err != nil {
		http.Error(w, "Project not found", http.StatusNotFound)
		return
	}

	// Update project scene with new prompt
	var sceneData []map[string]interface{}
	if err := json.Unmarshal([]byte(projectData.Scene), &sceneData); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if len(sceneData) > 0 {
		sceneData[0]["prompt"] = prompt
	}

	updatedScene, _ := json.Marshal(sceneData)
	if err := s.db.UpdateProjectScene(projectID, string(updatedScene)); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Generate code using AI
	conversation := NewCodeConversation(prompt, projectData.Source)
	var result JobSuccess
	var code string

Retry:
	for range 2 {
		code, err = conversation.Generate(s.groqClient)
		if err != nil {
			log.Printf("AI generation failed: %v", err)
			continue
		}

		log.Printf("AI generated code: %s", code)

		compileResult := s.compileQueue.Enqueue("use crate::simulo::*;\n" + code)
		switch compileResult.Status {
		case StatusSuccess:
			result = compileResult.Result.(JobSuccess)
			break Retry

		case StatusCompileError:
			conversation.ReportError(compileResult.Result.(string))
			log.Printf("Compile error: %s", compileResult.Result.(string))
			continue

		case StatusInternalError:
			log.Printf("Internal error: %s", compileResult.Result.(error).Error())
			break Retry
		}
	}

	if result.ID == "" {
		http.Error(w, "Job failed", http.StatusInternalServerError)
		return
	}

	if err := s.s3Client.UploadFile(result.ID, result.WasmPath); err != nil {
		log.Printf("WASM upload failed: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	result.Cleanup()

	// Save deployment
	if err := s.db.CreateDeployment(projectID, code, result.ID); err != nil {
		log.Printf("Failed to save deployment: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Write([]byte("OK"))
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w)

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	user, err := s.authorize(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	projects, err := s.db.GetUserProjects(user.ID)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(projects)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	ws := NewWebSocketHandler(s.db, s.supabaseCli, s.s3Client, conn)
	ws.Handle()
}

func (s *Server) parseProjectID(path string) string {
	re := regexp.MustCompile(`^/project/([^/]+)/agent$`)
	matches := re.FindStringSubmatch(path)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func (s *Server) authorize(r *http.Request) (*supabase.User, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, fmt.Errorf("no authorization header")
	}

	user, err := s.supabaseCli.Auth.User(context.Background(), auth)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	return user, nil
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w)

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authorize user
	user, err := s.authorize(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	project, err := s.db.CreateProject(user.ID)
	if err != nil {
		log.Printf("Failed to create project: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(project)
}

func (s *Server) renameProject(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w)

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	user, err := s.authorize(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse request body
	var request struct {
		ProjectID string `json:"project_id"`
		Name      string `json:"name"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// Validate input
	if request.ProjectID == "" {
		http.Error(w, "Project ID required", http.StatusBadRequest)
		return
	}

	if request.Name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}

	if len(request.Name) > 255 {
		http.Error(w, "Name too long", http.StatusBadRequest)
		return
	}

	err = s.db.RenameProject(request.ProjectID, user.ID, request.Name)
	if err != nil {
		if err.Error() == "project not found or not owned by user" {
			http.Error(w, "Project not found", http.StatusNotFound)
			return
		}
		log.Printf("Failed to rename project: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}