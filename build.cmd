@echo off
rem Windows GUI subsystem: no console window on launch (logs go to
rem %APPDATA%\ytm-client\vinylwin.log instead).
go build -ldflags "-s -w -H=windowsgui" -o ytm-client.exe .
rem SMTC integration runs out-of-process; the helper must sit next to the
rem main executable.
go build -ldflags "-s -w" -o wax-smtc.exe .\cmd\smtc-helper
