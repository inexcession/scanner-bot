# Scanner Bot

Scanner Bot is a Go-based automation tool designed to process scanned receipts and documents. It watches a specified directory for new files, uploads them to Google's Gemini API for analysis, and automatically renames and organizes them based on their content.

I made this because I get so many paper receipts and certificates from doctors etc that it's getting messy to keep all the papers together.

It's a tax season right now and I was manually typing in the medical receipts for deduction and well... I'm missing a few because I couldn't be arsed to stash them in the right place when I got them. I have my scanner set to scan directly to the folder that this bit of sofware watches.

It works :D

## Features

- **Automated Directory Watching**: Monitors a folder for new scans (PDF, JPG, PNG, JPEG), including sweeping existing files on startup.
- **AI-Powered Analysis**: Uses Google Gemini (`google.golang.org/genai`) with structured outputs to extract date, vendor, category, and total amount from receipts.
- **Smart Renaming**: Renames files to a standard format: `YYYY-MM-DD_Vendor_Amount円.ext` (with automatic collision numbering `_1`, `_2`, etc.).
- **Categorization**: Moves processed files into subdirectories based on their category (e.g., Grocery, Medical, Tax).
- **Multi-Receipt Support**: handling multiple receipts on a single page if recognized by the AI.
- **Originals Archiving**: Keeps the original raw scan in an `originals` folder.
- **Robustness**: Handles file stability checks (waiting for scanners to finish writing), context-aware graceful shutdown, and atomic/cross-device moves.

## Prerequisites

- Go 1.23 or later
- A Google Gemini API Key

## Installation

1.  **Clone the repository:**
    ```bash
    git clone https://github.com/inexcession/scanner-bot.git
    cd scanner-bot
    ```

2.  **Build the binary:**

    For your current machine:
    ```bash
    go build -o scanner-bot .
    ```

    To cross-compile for a Linux `amd64` server (e.g. from macOS Apple Silicon):
    ```bash
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o scanner-bot .
    ```

    To run the unit tests:
    ```bash
    go test -v ./...
    ```

3.  **Install (Linux/Systemd):**

    Edit the provided `scanner-bot.service` file to match your paths and user:

    ```ini
    [Service]
    User=your_user
    Environment="GEMINI_API_KEY=your_api_key_here"
    ExecStart=/path/to/scanner-bot -watch "/path/to/input" -dest "/path/to/output" -workers 2
    ```

    Then copy it to systemd and enable it:
    ```bash
    sudo cp scanner-bot.service /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable --now scanner-bot
    ```

    To view the logs, use journalctl:
    ```bash
    sudo journalctl -u scanner-bot -f
    ```

## Usage

You can run the bot manually from the command line:

```bash
export GEMINI_API_KEY="your_api_key_here"
./scanner-bot -watch "/path/to/watch/dir" -dest "/path/to/output/dir" -model "gemini-flash-latest" -workers 2
```

### Flags

- `-watch`: (Required) The directory to watch for new incoming scan files.
- `-dest`: (Required) The root directory where processed files and the `originals` folder will be created.
- `-model`: The Gemini model to use (default: "gemini-flash-latest").
- `-workers`: Maximum number of concurrent files to process, to prevent Gemini API rate limits (default: 2).

## How it Works

1.  **Detect**: The bot watches for `Create`, `Write`, or `Chmod` events in the watch directory.
2.  **Wait**: It waits for the file size to stabilize (indicating the scanner has finished writing).
3.  **Analyze**: The file is uploaded to Google Gemini.
4.  **Extract**: The AI extracts the Date (including Japanese era / Wareki conversion), Vendor, Category, Total Amount, and NTA medical expense fields (`医療費集計フォーム Ver.3.1`).
5.  **Process**:
    - For `Medical` receipts: Bundles the original scan (`<stem>.ext`) and a companion `<stem>.yaml` metadata file (containing all NTA medical form fields) into `dest/Medical/YYYY-MM-DD_Vendor_Amount円.zip`.
    - For all other categories: Copies the file to `dest/Category/YYYY-MM-DD_Vendor_Amount円.ext`.
    - Moves the raw original file to `dest/originals/filename.ext`.

## License

MIT
