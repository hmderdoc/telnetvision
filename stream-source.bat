@echo off
rem stream-source.bat — Windows companion to stream.sh for HD stream sources.
rem
rem Heavy decode + downscale happens in ffmpeg.exe (a native Win32 process
rem with its own address space), so Python only ever sees pre-shrunk BGR24
rem frames. Cuts producer.py's working set to single-MB territory regardless
rem of source resolution — necessary on 32-bit Python/Windows, helpful
rem everywhere else.
rem
rem Reads .env for BBS_HOST, BBS_PORT, TOKEN, plus optional COLS, ROWS, FPS,
rem TLS, INSECURE, CHANNEL, CAPTION_FILE, SOURCE.
rem SOURCE can also be passed as argv[1]. Env wins over .env wins over defaults.
rem
rem Usage:
rem   stream-source.bat                                     rem SOURCE from .env
rem   stream-source.bat http://192.168.0.23:5005/auto/v2.1  rem override SOURCE

setlocal EnableDelayedExpansion
cd /d "%~dp0"

if not exist .env (
  echo error: .env not found in "%CD%"
  exit /b 1
)

rem Parse .env: identifier=value lines only; strip a matching pair of
rem surrounding double-quotes (so TOKEN="abc" and TOKEN=abc both work);
rem don't clobber vars already in the environment.
for /F "usebackq tokens=1* delims==" %%A in (`findstr /R "^[A-Za-z_][A-Za-z0-9_]*=" .env`) do (
  set "KEY=%%A"
  set "VAL=%%B"
  if defined VAL (
    if "!VAL:~0,1!"=="""" if "!VAL:~-1!"=="""" set "VAL=!VAL:~1,-1!"
  )
  if not defined !KEY! set "!KEY!=!VAL!"
)

if not "%~1"=="" set "SOURCE=%~1"

if not defined SOURCE   ( echo error: SOURCE not set ^(env, .env, or argv[1]^) & exit /b 1 )
if not defined BBS_HOST ( echo error: BBS_HOST not set in .env                 & exit /b 1 )
if not defined BBS_PORT ( echo error: BBS_PORT not set in .env                 & exit /b 1 )
if not defined TOKEN    ( echo error: TOKEN not set in .env                    & exit /b 1 )

if not defined COLS    set "COLS=80"
if not defined ROWS    set "ROWS=24"
if not defined FPS     set "FPS=15"
if not defined CHANNEL set "CHANNEL=cam"

rem Pixel grid = COLS x 2*ROWS (one cell = two stacked half-block pixels).
set /a "IH=2*ROWS"

set "TLS_FLAG="
if "%TLS%"=="1"      set "TLS_FLAG=--tls"
set "INSECURE_FLAG="
if "%INSECURE%"=="1" set "INSECURE_FLAG=--insecure"
set "CAPTION_FLAG="
if defined CAPTION_FILE set CAPTION_FLAG=--caption-file "%CAPTION_FILE%"

rem Prefer the venv's python.exe if one's already set up next to the script —
rem it has the cv2/numpy/etc. that pip put there. No need to ".venv\Scripts\
rem activate" first (which, without `call`, would replace this batch and
rem abort the rest of the script).
set "VENVPY=%~dp0.venv\Scripts\python.exe"
if exist "%VENVPY%" (
  set "PY=%VENVPY%"
) else (
  set "PY=python"
  where %PY% >NUL 2>&1 || ( echo error: python not on PATH ^(or set up the venv: py -m venv .venv ^&^& .venv\Scripts\pip install -r requirements.txt^) & exit /b 1 )
)
where ffmpeg >NUL 2>&1 || ( echo error: ffmpeg not on PATH ^(install from https://www.gyan.dev/ffmpeg/builds/^) & exit /b 1 )

echo source : %SOURCE%
echo pixels : %COLS%x%IH%  (pre-downscaled by ffmpeg; Python never touches a full frame)
echo target : %BBS_HOST%:%BBS_PORT%  channel=%CHANNEL%  fps=%FPS%
echo.

ffmpeg -nostdin -loglevel error -fflags +discardcorrupt -err_detect ignore_err -i "%SOURCE%" -an -vf "scale=%COLS%:%IH%,fps=%FPS%" -pix_fmt bgr24 -f rawvideo - | %PY% producer.py --source - --in-size %COLS%x%IH% --host %BBS_HOST% --port %BBS_PORT% --token %TOKEN% --channel %CHANNEL% --cols %COLS% --rows %ROWS% --fps %FPS% --no-flip %TLS_FLAG% %INSECURE_FLAG% %CAPTION_FLAG%

endlocal
