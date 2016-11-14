/*
 * This file is part of remco.
 * © 2016 The Remco Authors
 *
 * For the full copyright and license information, please view the LICENSE
 * file that was distributed with this source code.
 */

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sync"

	"github.com/HeavyHorst/remco/config"
	"github.com/HeavyHorst/remco/log"
	"github.com/HeavyHorst/remco/template"
	"github.com/Sirupsen/logrus"
	"github.com/pborman/uuid"
)

type status struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Exec     config.Exec
	Template []*template.Processor
}

// Runner runs
type Runner struct {
	stoppedW         chan struct{}
	stopWatch        chan struct{}
	stopWatchConf    chan struct{}
	stoppedWatchConf chan struct{}
	reloadChan       chan struct{}
	wg               sync.WaitGroup
	mu               sync.Mutex
	canceled         bool
	configPath       string

	signalChans      map[string]chan os.Signal
	signalChansMutex sync.RWMutex

	resourceMap      map[string]status
	resourceMapMutex sync.RWMutex

	http    string
	pidFile string
}

// New creates a new Runner
func New(configPath string, done chan struct{}) (*Runner, error) {
	w := &Runner{
		stoppedW:      make(chan struct{}),
		stopWatch:     make(chan struct{}),
		stopWatchConf: make(chan struct{}),
		reloadChan:    make(chan struct{}),
		configPath:    configPath,
		signalChans:   make(map[string]chan os.Signal),
		resourceMap:   make(map[string]status),
	}

	cfg, err := config.NewConfiguration(configPath)
	if err != nil {
		return nil, err
	}

	w.http = cfg.Http
	w.pidFile = cfg.PidFile
	pid := os.Getpid()
	err = w.writePid(pid)
	if err != nil {
		log.WithFields(logrus.Fields{"pid_file": w.pidFile}).Error(err)
	}

	go w.runConfig(cfg)
	// go w.startWatchConfig(config)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-w.reloadChan:
				log.WithFields(logrus.Fields{
					"file": w.configPath,
				}).Info("loading new config")
				newConf, err := config.NewConfiguration(w.configPath)
				if err != nil {
					log.Error(err)
					continue
				}

				// don't try to relaod anything if w is already canceled
				if w.getCanceled() {
					continue
				}

				// write a new pidfile if the pid filepath has changed
				if newConf.PidFile != w.pidFile {
					err := w.deletePid()
					if err != nil {
						log.WithFields(logrus.Fields{"pid_file": w.pidFile}).Error(err)
					}
					w.pidFile = newConf.PidFile
					err = w.writePid(pid)
					if err != nil {
						log.WithFields(logrus.Fields{"pid_file": w.pidFile}).Error(err)
					}
				}

				// stop the old config and wait until it has stopped
				log.WithFields(logrus.Fields{
					"file": w.configPath,
				}).Info("stopping the old config")
				w.stopWatch <- struct{}{}
				<-w.stoppedW
				go w.runConfig(newConf)
				w.resetResourceMap()
			case <-w.stoppedW:
				// close the reloadChan
				// every attempt to write to reloadChan would block forever otherwise
				close(w.reloadChan)

				// close the done channel
				// this signals the main function that the Runner has completed all work
				// for example all backends are configured with onetime=true
				close(done)
				return
			}
		}
	}()

	return w, nil
}

func (ru *Runner) writePid(pid int) error {
	if ru.pidFile == "" {
		return nil
	}

	log.Info(fmt.Sprintf("creating pid file at %q", ru.pidFile))

	err := ioutil.WriteFile(ru.pidFile, []byte(fmt.Sprintf("%d", pid)), 0666)
	if err != nil {
		return fmt.Errorf("could not create pid file: %s", err)
	}
	return nil
}

func (ru *Runner) deletePid() error {
	if ru.pidFile == "" {
		return nil
	}

	log.Debug(fmt.Sprintf("removing pid file at %q", ru.pidFile))

	stat, err := os.Stat(ru.pidFile)
	if err != nil {
		return fmt.Errorf("could not remove pid file: %s", err)
	}

	if stat.IsDir() {
		return fmt.Errorf("the pid file path seems to be a directory")
	}

	return os.Remove(ru.pidFile)
}

func (ru *Runner) addSignalChan(id string, sigchan chan os.Signal) {
	ru.signalChansMutex.Lock()
	defer ru.signalChansMutex.Unlock()
	ru.signalChans[id] = sigchan
}

func (ru *Runner) removeSignalChan(id string) {
	ru.signalChansMutex.Lock()
	defer ru.signalChansMutex.Unlock()
	delete(ru.signalChans, id)
}

// SendSignal forwards the given Signal to all child processes
func (ru *Runner) SendSignal(s os.Signal) {
	ru.signalChansMutex.RLock()
	defer ru.signalChansMutex.RUnlock()
	for _, v := range ru.signalChans {
		v <- s
	}
}

func (ru *Runner) setResource(id, state string, r config.Resource) {
	ru.resourceMapMutex.Lock()
	defer ru.resourceMapMutex.Unlock()
	ru.resourceMap[id] = status{
		ID:       id,
		Name:     r.Name,
		State:    state,
		Exec:     r.Exec,
		Template: r.Template,
	}
}

func (ru *Runner) resetResourceMap() {
	ru.resourceMapMutex.Lock()
	defer ru.resourceMapMutex.Unlock()
	ru.resourceMap = make(map[string]status)
}

func (ru *Runner) Status() ([]byte, error) {
	ru.resourceMapMutex.RLock()
	defer ru.resourceMapMutex.RUnlock()
	vs := make([]status, len(ru.resourceMap))

	idx := 0
	for _, v := range ru.resourceMap {
		vs[idx] = v
		idx++
	}
	dat, err := json.MarshalIndent(vs, "", "    ")
	return dat, err
}

func (ru *Runner) runConfig(c config.Configuration) {
	defer func() {
		ru.stoppedW <- struct{}{}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})

	wait := sync.WaitGroup{}
	for _, v := range c.Resource {
		wait.Add(1)
		go func(r config.Resource) {
			defer wait.Done()
			res, err := r.Init(ctx)
			if err != nil {
				log.Error(err)
			} else {
				defer res.Close()
				id := uuid.New()
				ru.setResource(id, "running", r)
				ru.addSignalChan(id, res.SignalChan)
				defer ru.removeSignalChan(id)
				defer ru.setResource(id, "stopped", r)
				res.Monitor(ctx)
			}
		}(v)
	}

	go func() {
		// If there is no goroutine left - quit
		wait.Wait()
		close(done)
	}()

	for {
		select {
		case <-ru.stopWatch:
			cancel()
			wait.Wait()
			return
		case <-done:
			return
		}
	}
}

func (ru *Runner) getCanceled() bool {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	return ru.canceled
}

func (ru *Runner) StartStatusHandler() {
	go func() {
		http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			s, _ := ru.Status()
			w.Write(s)
		})
		if ru.http != "" {
			http.ListenAndServe(ru.http, nil)
		}
	}()
}

// Reload rereads the configuration, stops the old Runner and starts a new one.
func (ru *Runner) Reload() {
	ru.reloadChan <- struct{}{}
}

// Stop stops the Runner gracefully.
func (ru *Runner) Stop() {
	ru.mu.Lock()
	ru.canceled = true
	ru.mu.Unlock()
	close(ru.stopWatch)
	close(ru.stopWatchConf)

	// wait for the main routine and startWatchConfig to exit
	ru.wg.Wait()

	// remove the pidfile
	err := ru.deletePid()
	if err != nil {
		log.WithFields(logrus.Fields{"pid_file": ru.pidFile}).Error(err)
	}
}