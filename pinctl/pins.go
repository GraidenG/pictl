package pinctl

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/warthog618/go-gpiocdev"
	"github.com/warthog618/go-gpiocdev/device/rpi"
)

const (
	pwrPin   = rpi.GPIO16
	rstPin   = rpi.GPIO17
	ledPin   = rpi.GPIO22
	gpiochip = "gpiochip0" // the device name from linux
	consumer = "pictl"     // identifier for pin ownership
)

// How long each kind of press holds the line high. Exported so callers can
// report the duration to a UI without duplicating the number.
const (
	ShortPressDuration = 500 * time.Millisecond
	LongPressDuration  = 6 * time.Second
)

// generic pin type
type pin struct {
	line *gpiocdev.Line
	name string // name of the pin, used for logging and debugging
}

// output pin type
type outputPin struct {
	pin
	mu sync.Mutex // mostly used to prevent duplicate requests via TryLock since it is imitating a physical button
}

// input pin type
type inputPin struct {
	pin
	currentStatus atomic.Bool // atomic because it will be accessed by multiple goroutines, but shouldn't block any (i.e. the HTTP api reading this just needs a value, it doesn't need to be blocked by an incoming update)
}

// the pins that are used
var (
	PowerSwitch outputPin
	ResetSwitch outputPin
	PowerLED    inputPin
)

// can be identified using errors.Is, even if wrapped in another error
var (
	// Returned when a button is already pressed (mutex lock active) and another press is attempted
	ErrButtonAlreadyPressed = errors.New("button is already pressed")
)

// long button press, i.e. simulate holding the button down
func (p *outputPin) LongPress() error {
	return p.press(LongPressDuration)
}

// short button press, i.e. simulate a single press of the button
func (p *outputPin) ShortPress() error {
	return p.press(ShortPressDuration)
}

// Button press functionality utilizing a mutex to ensure conflicts don't occur (since it is trying to imitate a physical button, conflicts don't make sense)
func (p *outputPin) press(duration time.Duration) error {
	if success := p.mu.TryLock(); !success {
		return fmt.Errorf("%s: %w", p.name, ErrButtonAlreadyPressed)
	}
	defer p.mu.Unlock()

	err := p.setHigh()
	if err != nil {
		// as far as I can tell there's no situation where setHigh can return an error while still having succeeded.
		return err
	}

	time.Sleep(duration)
	err = p.setLow()
	if err != nil {
		return err
	}
	return nil
}

func (p *outputPin) setHigh() error {
	err := p.line.SetValue(1)
	if err != nil {
		return fmt.Errorf("setting pin high: %w", err)
	}
	return nil
}

// Try to set the pin low, if this fails it is critical that it does not get stuck high and the error handling will go as
// far as crashing the program to ensure it gets closed (and set to input by the kernel when reclaimed)
func (p *outputPin) setLow() error {
	err := p.line.SetValue(0)
	if err != nil {
		if closeErr := p.closePin(); closeErr != nil && !errors.Is(closeErr, gpiocdev.ErrClosed) {
			// give up and crash so the kernel sets the pin low
			// Note: this is skipped if the error was that the line was already closed
			log.Fatalf("setting %s pin low: %v (fatal: %v)", p.name, err, closeErr)
		}
		return fmt.Errorf("setting %s pin low: %w", p.name, err)
	}
	return nil
}

func (p *inputPin) GetStatus() bool {
	return p.currentStatus.Load()
}

func (p *inputPin) UpdateStatus() error {
	val, err := p.line.Value()
	if err != nil {
		return err
	}

	booleanValue := !(val == 0)
	p.currentStatus.Store(booleanValue)
	return nil
}

// SyncValue prevents desyncs of the inputPin's currentStatus boolean that are definitely unlikely, but I don't think are impossible
func (p *inputPin) SyncValue(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// if the context is over, return
			return
		case <-ticker.C:
			// every tick, update the value to current
			err := p.UpdateStatus()
			if err != nil {
				log.Printf("updating %s pin status: %v", p.name, err)
			}
		}
	}

}

// handle edge detection for input pins using logical active level
// The way these are read ensures that while some events may be dropped from the buffer
// if they are not read, the newest event will always be present. So ultimately, it will
// always end up correct but may miss intermediate events (this can be detected using
// sequence numbers if needed).
func (p *inputPin) handleEdge(evt gpiocdev.LineEvent) {
	if evt.Type == gpiocdev.LineEventFallingEdge {
		p.currentStatus.Store(false)
	} else {
		p.currentStatus.Store(true)
	}
}

// Initialize requests every Line in layout with the corresponding config and adds to the Pins map.
// In the case of failure, it will make an effort to close any that were opened
func Initialize() error {
	var err error
	// init power switch as output low
	if PowerSwitch.pin, err = requestPin("power_switch", pwrPin, gpiocdev.AsOutput(0)); err != nil {
		return err
	}

	// init reset switch as output low
	if ResetSwitch.pin, err = requestPin("reset_switch", rstPin, gpiocdev.AsOutput(0)); err != nil {
		return err
	}

	// init powerLED input as input, active low (inverted, connected to ground = high since that represents the LED), internal pull up resistor enabled, and edge detection
	if PowerLED.pin, err = requestPin("power_led", ledPin, gpiocdev.AsInput, gpiocdev.AsActiveLow, gpiocdev.WithPullUp, gpiocdev.WithBothEdges, gpiocdev.WithEventHandler(PowerLED.handleEdge)); err != nil {
		return err
	}
	err = PowerLED.UpdateStatus()
	if err != nil {
		return err
	}

	return nil
}

// list of all successfully created lines that haven't yet been closed
// no current purpose outside of usage with CloseAll but I wanted to keep track of this independently
var pins []pin

func requestPin(name string, offset int, opts ...gpiocdev.LineReqOption) (pin, error) {
	// consumer is label used to identify the process using a pin, always apply the pictl one
	opts = append([]gpiocdev.LineReqOption{gpiocdev.WithConsumer(consumer)}, opts...)
	line, err := gpiocdev.RequestLine(gpiochip, offset, opts...)
	if err != nil {
		return pin{}, fmt.Errorf("requesting %s pin (offset %d): %w", name, offset, err)
	}
	p := pin{name: name, line: line}
	pins = append(pins, p)
	return p, nil
}

// CloseAll releases every Line back to the kernel.
func CloseAll() error {
	var errs []error
	var failed []pin
	for _, p := range pins {
		if err := p.closePin(); err != nil && !errors.Is(err, gpiocdev.ErrClosed) {
			errs = append(errs, err)
			failed = append(failed, p) // keep track of pins that failed to close
		}
	}

	// keep track of any pins that failed to close
	pins = failed
	return errors.Join(errs...)
}

// close a pin
func (p pin) closePin() error {
	if err := p.line.Close(); err != nil {
		return fmt.Errorf("closing pin: %w", err)
	}
	return nil
}
