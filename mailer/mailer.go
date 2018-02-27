package mailer

import (
	"context"
	"errors"
	"io"
	"log"
	"net/textproto"
	"os"
	"time"

	"github.com/gophish/gomail"
)

var MailChunkSize = 10
var MailDelayTime = 10 * time.Minute

// MaxReconnectAttempts is the maximum number of times we should reconnect to a server
var MaxReconnectAttempts = 10

// ErrMaxConnectAttempts is thrown when the maximum number of reconnect attempts
// is reached.
var ErrMaxConnectAttempts = errors.New("max connection attempts reached")

// Logger is the logger for the worker
var Logger = log.New(os.Stdout, " ", log.Ldate|log.Ltime|log.Lshortfile)

// Sender exposes the common operations required for sending email.
type Sender interface {
	Send(from string, to []string, msg io.WriterTo) error
	Close() error
	Reset() error
}

// Dialer dials to an SMTP server and returns the SendCloser
type Dialer interface {
	Dial() (Sender, error)
}

// Mail is an interface that handles the common operations for email messages
type Mail interface {
	Backoff(reason error) error
	Error(err error) error
	Success() error
	Generate(msg *gomail.Message) error
	GetDialer() (Dialer, error)
}

// Mailer is a global instance of the mailer that can
// be used in applications. It is the responsibility of the application
// to call Mailer.Start()
var Mailer *MailWorker

func init() {
	Mailer = NewMailWorker()
}

// MailWorker is the worker that receives slices of emails
// on a channel to send. It's assumed that every slice of emails received is meant
// to be sent to the same server.
type MailWorker struct {
	Queue chan []Mail
}

// NewMailWorker returns an instance of MailWorker with the mail queue
// initialized.
func NewMailWorker() *MailWorker {
	return &MailWorker{
		Queue: make(chan []Mail),
	}
}

// Start launches the mail worker to begin listening on the Queue channel
// for new slices of Mail instances to process.
func (mw *MailWorker) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ms := <-mw.Queue:
			go func(ctx context.Context, ams []Mail) {
				Logger.Printf("Mailer got %d mail to send", len(ams))

				for len(ams) > MailChunkSize {
					ms := ams[:MailChunkSize]
					dialer, err := ms[0].GetDialer()
					if err != nil {
						errorMail(err, ms)
						return
					}
					sendMail(ctx, dialer, ms)
					time.Sleep(MailDelayTime)
					ams = ams[MailChunkSize:]
				}

				if len(ams) == 0 {
					return
				}

				dialer, err := ams[0].GetDialer()
				if err != nil {
					errorMail(err, ams)
					return
				}
				sendMail(ctx, dialer, ams)
			}(ctx, ms)
		}
	}
}

// errorMail is a helper to handle erroring out a slice of Mail instances
// in the case that an unrecoverable error occurs.
func errorMail(err error, ms []Mail) {
	for _, m := range ms {
		m.Error(err)
	}
}

// dialHost attempts to make a connection to the host specified by the Dialer.
// It returns MaxReconnectAttempts if the number of connection attempts has been
// exceeded.
func dialHost(ctx context.Context, dialer Dialer) (Sender, error) {
	sendAttempt := 0
	var sender Sender
	var err error
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		default:
			break
		}
		sender, err = dialer.Dial()
		if err == nil {
			break
		}
		sendAttempt++
		if sendAttempt == MaxReconnectAttempts {
			err = ErrMaxConnectAttempts
			break
		}
	}
	return sender, err
}

// sendMail attempts to send the provided Mail instances.
// If the context is cancelled before all of the mail are sent,
// sendMail just returns and does not modify those emails.
func sendMail(ctx context.Context, dialer Dialer, ms []Mail) {
	sender, err := dialHost(ctx, dialer)
	if err != nil {
		errorMail(err, ms)
		return
	}
	defer sender.Close()
	message := gomail.NewMessage()
	for _, m := range ms {
		select {
		case <-ctx.Done():
			return
		default:
			break
		}
		message.Reset()

		err = m.Generate(message)
		if err != nil {
			m.Error(err)
			continue
		}

		err = gomail.Send(sender, message)
		if err != nil {
			if te, ok := err.(*textproto.Error); ok {
				switch {
				// If it's a temporary error, we should backoff and try again later.
				// We'll reset the connection so future messages don't incur a
				// different error (see https://github.com/gophish/gophish/issues/787).
				case te.Code >= 400 && te.Code <= 499:
					m.Backoff(err)
					sender.Reset()
					continue
				// Otherwise, if it's a permanent error, we shouldn't backoff this message,
				// since the RFC specifies that running the same commands won't work next time.
				// We should reset our sender and error this message out.
				case te.Code >= 500 && te.Code <= 599:
					m.Error(err)
					sender.Reset()
					continue
				// If something else happened, let's just error out and reset the
				// sender
				default:
					m.Error(err)
					sender.Reset()
					continue
				}
			} else {
				m.Error(err)
				sender.Reset()
				continue
			}
		}
		m.Success()
	}
}
