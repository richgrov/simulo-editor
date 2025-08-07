package main

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
	"github.com/nedpals/supabase-go"
)

type DatabaseClient struct {
	db          *sql.DB
	supabase    *supabase.Client
	supabaseKey string
}

type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ProjectWithDeployment struct {
	ID        string `json:"id"`
	Scene     string `json:"scene"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
}

type Machine struct {
	ID        int    `json:"id"`
	PublicKey string `json:"public_key"`
}

type MachineProject struct {
	Scene          string `json:"scene"`
	CompiledObject string `json:"compiled_object"`
}

type ProjectData struct {
	Owner string `json:"owner"`
	Scene string `json:"scene"`
}

func NewDatabaseClient(postgresURL, supabaseURL, supabaseKey string) (*DatabaseClient, error) {
	db, err := sql.Open("postgres", postgresURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	supabaseClient := supabase.CreateClient(supabaseURL, supabaseKey)

	return &DatabaseClient{
		db:          db,
		supabase:    supabaseClient,
		supabaseKey: supabaseKey,
	}, nil
}

func (d *DatabaseClient) GetUserProjects(userID string) ([]Project, error) {
	query := "SELECT id, name FROM projects WHERE owner = $1"
	rows, err := d.db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to query projects: %w", err)
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var project Project
		if err := rows.Scan(&project.ID, &project.Name); err != nil {
			return nil, fmt.Errorf("failed to scan project: %w", err)
		}
		projects = append(projects, project)
	}

	return projects, nil
}

func (d *DatabaseClient) GetProjectWithLatestDeployment(projectID string) (*ProjectWithDeployment, error) {
	query := `
		SELECT p.id, p.scene, COALESCE(d.source, '') as source, COALESCE(d.created_at::text, '') as created_at
		FROM projects p
		LEFT JOIN deployments d ON p.id = d.project_id
		WHERE p.id = $1
		ORDER BY d.created_at DESC
		LIMIT 1
	`

	var result ProjectWithDeployment
	err := d.db.QueryRow(query, projectID).Scan(
		&result.ID,
		&result.Scene,
		&result.Source,
		&result.CreatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("project not found")
		}
		return nil, fmt.Errorf("failed to get project: %w", err)
	}

	return &result, nil
}

func (d *DatabaseClient) UpdateProjectScene(projectID, scene string) error {
	query := "UPDATE projects SET scene = $1 WHERE id = $2"
	_, err := d.db.Exec(query, scene, projectID)
	if err != nil {
		return fmt.Errorf("failed to update project scene: %w", err)
	}
	return nil
}

func (d *DatabaseClient) CreateDeployment(projectID, source, compiledObject string) error {
	query := "INSERT INTO deployments (project_id, source, compiled_object) VALUES ($1, $2, $3)"
	_, err := d.db.Exec(query, projectID, source, compiledObject)
	if err != nil {
		return fmt.Errorf("failed to create deployment: %w", err)
	}
	return nil
}

func (d *DatabaseClient) GetMachine(machineID int) (*Machine, error) {
	query := "SELECT id, public_key FROM machines WHERE id = $1"

	var machine Machine
	err := d.db.QueryRow(query, machineID).Scan(&machine.ID, &machine.PublicKey)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("machine not found")
		}
		return nil, fmt.Errorf("failed to get machine: %w", err)
	}

	return &machine, nil
}

func (d *DatabaseClient) GetMachineProject(machineID int) (*MachineProject, error) {
	query := `
		SELECT p.scene, d.compiled_object
		FROM machines m
		JOIN projects p ON p.id = m.project
		JOIN deployments d ON d.project_id = p.id
		WHERE m.id = $1
		ORDER BY d.created_at DESC
		LIMIT 1
	`

	var result MachineProject
	err := d.db.QueryRow(query, machineID).Scan(&result.Scene, &result.CompiledObject)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("machine project not found")
		}
		return nil, fmt.Errorf("failed to get machine project: %w", err)
	}

	return &result, nil
}

func (d *DatabaseClient) GetProject(projectID string) (*ProjectData, error) {
	query := "SELECT owner, scene FROM projects WHERE id = $1"

	var project ProjectData
	err := d.db.QueryRow(query, projectID).Scan(&project.Owner, &project.Scene)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("project not found")
		}
		return nil, fmt.Errorf("failed to get project: %w", err)
	}

	return &project, nil
}

func (d *DatabaseClient) CreateProject(userID string) (*Project, error) {
	query := "INSERT INTO projects (id, name, owner, scene) VALUES (gen_random_uuid(), $1, $2, $3) RETURNING id, name"

	var project Project
	err := d.db.QueryRow(query, "Unnamed Project", userID, "[]").Scan(&project.ID, &project.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}

	return &project, nil
}

func (d *DatabaseClient) RenameProject(projectID, userID, newName string) error {
	query := "UPDATE projects SET name = $1 WHERE id = $2 AND owner = $3"

	result, err := d.db.Exec(query, newName, projectID, userID)
	if err != nil {
		return fmt.Errorf("failed to rename project: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("project not found or not owned by user")
	}

	return nil
}