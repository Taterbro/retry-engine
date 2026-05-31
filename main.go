package main

import (
	"log"
	"net/http"
)

func main() {
	// 1. Open (or create) retry.db in the current directory.
	db := initDB("retry.db")
	defer db.Close()

	// 2. Start the background worker in its own goroutine.
	worker := NewWorker(db)
	go worker.Run()

	// 3. Register routes.
	//    Go 1.22+ supports "METHOD /path" patterns natively in http.ServeMux.
	h := NewHandlers(db)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /request", h.CreateRequest)
	mux.HandleFunc("GET /requests/{id}", h.GetRequest) // {id} is a path param
	mux.HandleFunc("GET /requests", h.ListRequests)

	log.Println("Retry engine listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
