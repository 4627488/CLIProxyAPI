package auth

import (
	"sync"
	"testing"
)

func TestCredentialUpdatesConcurrentWithResults(t *testing.T) {
	for _, operation := range []string{"register", "update", "refresh"} {
		t.Run(operation, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			_, err := manager.Register(t.Context(), &Auth{ID: "concurrent", Provider: "codex", Status: StatusActive})
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var workers sync.WaitGroup
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				for range 500 {
					manager.MarkResult(t.Context(), Result{AuthID: "concurrent", Provider: "codex", Model: "gpt-6-sol", Success: true})
				}
			}()
			close(start)
			for range 500 {
				base, _ := manager.GetByID("concurrent")
				switch operation {
				case "register":
					_, err = manager.Register(t.Context(), base)
				case "update":
					_, err = manager.Update(t.Context(), base)
				case "refresh":
					_, err = manager.UpdateRefreshedAuth(t.Context(), base, base.Clone())
				}
				if err != nil {
					t.Error(err)
					break
				}
			}
			workers.Wait()
		})
	}
}
