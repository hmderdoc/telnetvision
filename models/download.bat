@echo off
rem Windows companion to download.sh — no bash / no Git Bash required.
rem Usage:
rem   download.bat            # base.en   (~142 MB, default)
rem   download.bat small.en   # better accuracy (~466 MB)
rem   download.bat tiny.en    # fastest / lowest latency (~75 MB)
rem
rem curl.exe ships in C:\Windows\System32 on Windows 10 (1803+) and 11.
rem On older Windows: open PowerShell and run
rem   Invoke-WebRequest -Uri https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin -OutFile models\ggml-base.en.bin

setlocal
cd /d "%~dp0"

if "%~1"=="" (set "MODEL=base.en") else (set "MODEL=%~1")
set "URL=https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-%MODEL%.bin"

echo downloading ggml-%MODEL%.bin from huggingface ...
curl -L --fail -o "ggml-%MODEL%.bin" "%URL%"
if errorlevel 1 (
  echo.
  echo download failed. If curl is missing, use the PowerShell command in the header.
  exit /b 1
)
echo saved models\ggml-%MODEL%.bin
endlocal
