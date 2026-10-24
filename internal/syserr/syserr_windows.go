package syserr

import "syscall"

// Windows error codes (winerror.h / winsock2.h).
var (
	winNoSpace      = []syscall.Errno{112, 39} // ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL
	winConnRefused  = []syscall.Errno{10061}   // WSAECONNREFUSED
	winNoRoute      = []syscall.Errno{10051, 10065, 10050}
	winAddrNotAvail = []syscall.Errno{10049} // WSAEADDRNOTAVAIL
	winMsgSize      = []syscall.Errno{10040} // WSAEMSGSIZE
)
