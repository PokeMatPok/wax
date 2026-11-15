@echo off
rem Windows GUI subsystem: no console window on launch (logs go to
rem %APPDATA%\ytm-client\vinylwin.log instead).
go build -ldflags "-s -w -H=windowsgui" -o ytm-client.exe .
