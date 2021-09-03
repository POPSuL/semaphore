package tasks

import (
	"time"

	"github.com/ansible-semaphore/semaphore/db"
)

type scheduler struct {
	store db.Store
}

func CreateScheduler(store db.Store) scheduler {
	return scheduler{
		store: store,
	}
}

func (s *scheduler) wakeUp() {

}

//func (s *scheduler)

func (s *scheduler) Run() {
	s.wakeUp()
	for {
		time.Sleep(1 * time.Second)
	}
}

func (s *scheduler) Stop() {

}
