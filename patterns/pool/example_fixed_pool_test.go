package pool

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/mingkhuang/go-concurrency-limits/limit"
)

func ExampleFixedPool() {
	type JobKey string
	var JobKeyID = JobKey("job_id")

	l := 1000 // limit to 1000 concurrent requests.
	// create a new pool
	pool, err := NewFixedPool(
		"protected_resource_pool",
		OrderingRandom,
		l,
		100,
		time.Millisecond*250,
		time.Millisecond*500,
		time.Millisecond*10,
		0,
		time.Second,
		limit.BuiltinLimitLogger{},
		nil,
	)
	if err != nil {
		panic(err)
	}

	wg := sync.WaitGroup{}
	wg.Add(l * 3)
	// spawn 3000 concurrent requests that would normally be too much load for the protected resource.
	for i := 0; i <= l*3; i++ {
		go func(c int) {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), JobKeyID, c)
			// this will block until timeout or token was acquired.
			listener, ok := pool.Acquire(ctx)
			if !ok {
				log.Printf("was not able to acquire lock for id=%d\n", c)
				return
			}
			log.Printf("acquired lock for id=%d\n", c)
			// do something...
			time.Sleep(time.Millisecond * 10)
			listener.OnSuccess()
			log.Printf("released lock for id=%d\n", c)
		}(i)
	}

	// wait for completion
	wg.Wait()
	log.Println("Finished")
}
