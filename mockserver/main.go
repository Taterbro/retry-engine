package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

var count int32

func handler(w http.ResponseWriter, r *http.Request) {
	n := atomic.AddInt32(&count, 1)

	fmt.Println("Request:", n)

	if n <= 3 {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("fail"))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("success"))
}

func main() {
	http.HandleFunc("/test", handler)
	http.ListenAndServe(":8081", nil)
}
