@echo off
rem Runs at the end of dockur/windows' unattended install (C:\OEM is the oem\ folder).
powershell -NoProfile -ExecutionPolicy Bypass -File C:\OEM\setup.ps1 > Z:\setup-log.txt 2>&1
