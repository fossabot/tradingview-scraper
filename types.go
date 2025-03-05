package tradingview

import "time"

// SocketInterface ...
type SocketInterface interface {
	AddSymbol(symbol string) error
	RemoveSymbol(symbol string) error
	Init() error
	Close() error
}

// SocketMessage ...
type SocketMessage struct {
	Message string      `json:"m"`
	Payload interface{} `json:"p"`
}

// QuoteMessage ...
type QuoteMessage struct {
	Symbol string     `mapstructure:"n"`
	Status string     `mapstructure:"s"`
	Data   *QuoteData `mapstructure:"v"`
}

// QuoteData ...
type QuoteData struct {
	Date   time.Time `json:"date"`
	Symbol string    `json:"symbol"`
	Source string    `json:"source"`
	Price  *float64  `json:"price" mapstructure:"lp"`
	Volume *float64  `json:"volume" mapstructure:"volume"`
	Bid    *float64  `json:"bid" mapstructure:"bid"`
	Ask    *float64  `json:"ask" mapstructure:"ask"`
}

// Flags ...
type Flags struct {
	Flags []string `json:"flags"`
}

// OnReceiveDataCallback ...
type OnReceiveDataCallback func(quote *QuoteData)

// OnErrorCallback ...
type OnErrorCallback func(err error, context string)
