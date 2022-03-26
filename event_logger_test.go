package astiencoder

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type mockedLogger struct {
	m    *sync.Mutex
	msgs map[string]int
}

func newMockedLogger() *mockedLogger {
	return &mockedLogger{
		m:    &sync.Mutex{},
		msgs: make(map[string]int),
	}
}

func (l *mockedLogger) Fatal(v ...interface{}) {
	l.m.Lock()
	defer l.m.Unlock()
	l.msgs[fmt.Sprint(v...)]++
	os.Exit(1)
}
func (l *mockedLogger) Fatalf(format string, v ...interface{}) {
	l.m.Lock()
	defer l.m.Unlock()
	l.msgs[fmt.Sprintf(format, v...)]++
	os.Exit(1)
}
func (l *mockedLogger) Print(v ...interface{}) {
	l.m.Lock()
	defer l.m.Unlock()
	l.msgs[fmt.Sprint(v...)]++
}
func (l *mockedLogger) Printf(format string, v ...interface{}) {
	l.m.Lock()
	defer l.m.Unlock()
	l.msgs[fmt.Sprintf(format, v...)]++
}

func TestEventLogger(t *testing.T) {
	ml := newMockedLogger()
	l := newEventLogger(ml)
	WithMessageMerging(500*time.Millisecond)(nil, l)
	l.Start(context.Background())
	go func() {
		l.Errorf("errorf-%d", 1)
		l.Errorf("errorf-%d", 1)
		l.Errorf("errorf-%d", 2)
		l.Errorf("errorf-%d", 3)
		l.Errorf("errorf-%d", 3)
		l.Errorf("errorf-%d", 3)
		l.Infof("infof-%d", 1)
		l.Infof("infof-%d", 1)
		l.Infof("infof-%d", 2)
		l.Infof("infof-%d", 3)
		l.Infof("infof-%d", 3)
		l.Infof("infof-%d", 3)
		l.Debugk("debugk-%d", "debugk-1")
		l.Debugk("debugk-%d", "debugk-2")
		l.Debugk("debugk-%d", "debugk-3")
		l.Errork("errork-%d", "errork-1")
		l.Errork("errork-%d", "errork-2")
		l.Errork("errork-%d", "errork-3")
		l.Infok("infok-%d", "infok-1")
		l.Infok("infok-%d", "infok-2")
		l.Infok("infok-%d", "infok-3")
		l.Warnk("warnk-%d", "warnk-1")
		l.Warnk("warnk-%d", "warnk-2")
		l.Warnk("warnk-%d", "warnk-3")
		l.Errorf("msg")
		l.Errorf("msg")
		l.Infof("msg")
		l.Infof("msg")
	}()
	time.Sleep(time.Second)
	ml.m.Lock()
	require.Equal(t, map[string]int{
		"astiencoder: pattern repeated once: errorf-1":     1,
		"astiencoder: pattern repeated once: infof-1":      1,
		"astiencoder: pattern repeated 2 times: debugk-%d": 1,
		"astiencoder: pattern repeated 2 times: errork-%d": 1,
		"astiencoder: pattern repeated 2 times: errorf-3":  1,
		"astiencoder: pattern repeated 2 times: infok-%d":  1,
		"astiencoder: pattern repeated 2 times: infof-3":   1,
		"astiencoder: pattern repeated once: msg":          2,
		"astiencoder: pattern repeated 2 times: warnk-%d":  1,
		"debugk-1": 1,
		"errork-1": 1,
		"errorf-1": 1,
		"errorf-2": 1,
		"errorf-3": 1,
		"infok-1":  1,
		"infof-1":  1,
		"infof-2":  1,
		"infof-3":  1,
		"msg":      2,
		"warnk-1":  1,
	}, ml.msgs)
	ml.msgs = map[string]int{}
	ml.m.Unlock()
	l.Infof("purge-%d", 1)
	l.Infof("purge-%d", 1)
	l.Infof("purge-%d", 1)
	l.Close()
	ml.m.Lock()
	require.Equal(t, map[string]int{
		"astiencoder: pattern repeated 2 times: purge-1": 1,
		"purge-1": 1,
	}, ml.msgs)
	ml.m.Unlock()
}
