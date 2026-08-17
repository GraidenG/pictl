package pinctl

import (
	"errors"
	"fmt"
	"log"
	"sync"
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

// generic pin type
type pin struct {
	line *gpiocdev.Line
	name string // name of the pin, used for logging and debugging
}

// output pin type
type outputPin struct {
	pin
	mu sync.Mutex
}

// input pin type
type inputPin struct {
	pin
	currentStatus bool
}

// the pins that are used
var (
	PowerSwitch outputPin
	ResetSwitch outputPin
	PowerLED    inputPin
)

// can be identified using errors.Is, even if wrapped in another error
var (
	ErrButtonAlreadyPressed = errors.New("button is already pressed")
)

// long button press, i.e. simulate holding the button down
func (p *outputPin) LongPress() error {
	return p.press(6 * time.Second)
}

// short button press, i.e. simulate a single press of the button
func (p *outputPin) ShortPress() error {
	return p.press(500 * time.Millisecond)
}

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

func (p *outputPin) setLow() error {
	err := p.line.SetValue(0)
	if err != nil {
		if closeErr := p.closePin(); closeErr != nil {
			// give up and crash so the kernel sets the pin low
			log.Fatalf("setting %s pin low: %v (fatal: %v)", p.name, err, closeErr)
		}
		return fmt.Errorf("setting %s pin low: %w", p.name, err)
	}
	return nil
}

func (p *inputPin) GetStatus() bool {
	return p.currentStatus
}

// Initialize requests every Line in layout with the corresponding config and adds to the Pins map.
// In the case of failure, it will make an effort to close any that were opened
func Initialize() error {
	var err error
	if PowerSwitch.pin, err = requestPin("power_switch", pwrPin, gpiocdev.AsOutput(0)); err != nil {
		return err
	}

	if ResetSwitch.pin, err = requestPin("reset_switch", rstPin, gpiocdev.AsOutput(0)); err != nil {
		return err
	}

	if PowerLED.pin, err = requestPin("power_led", ledPin, gpiocdev.AsInput, gpiocdev.WithPullUp); err != nil {
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
		if err := p.closePin(); err != nil {
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
