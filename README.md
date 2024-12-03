[![GoDoc](https://godoc.org/github.com/knieriem/serport?status.svg)](https://godoc.org/github.com/knieriem/serframe)


# serframe

This package provides a mechanism to read byte frames from a serial stream.

It is currently used to implement binary protocols like Modbus (UART, TCP, CAN)
or HSLI (high-speed lighting interface, UARToverCAN, as used in
the [TLD7002-16es] LED driver).

Another use case is reading frames from a text-based command-line interface.

[TLD7002-16ES]: https://www.infineon.com/cms/de/product/power/lighting-ics/litix-automotive-led-driver-ic/litix-pixel-rear/tld7002-16es/
