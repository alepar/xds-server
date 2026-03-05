#!/usr/bin/env Rscript
# analyze.R — Read bench CSV, print 100ms bucketed stats around swap event
#
# Usage: Rscript analyze.R bench-result-legacy.csv bench-result-fixed.csv

args <- commandArgs(trailingOnly = TRUE)
if (length(args) < 1) {
  cat("Usage: Rscript analyze.R <file1.csv> [file2.csv ...]\n")
  quit(status = 1)
}

read_bench <- function(path) {
  # Parse swap_ms from comment line
  first_line <- readLines(path, n = 1)
  swap_ms <- as.numeric(sub(".*swap_ms=", "", first_line))

  df <- read.csv(path, comment.char = "#")
  list(data = df, swap_ms = swap_ms, label = sub("bench-result-(.*)\\.csv", "\\1", basename(path)))
}

pct <- function(x, p) {
  if (length(x) == 0) return(NA)
  quantile(x, p, names = FALSE)
}

analyze_one <- function(bench, window_s = 2.0) {
  df <- bench$data
  swap_ms <- bench$swap_ms
  label <- bench$label

  # Bucket by 100ms
  df$bucket <- floor(df$start_ms / 100) * 100

  # Filter to window around swap
  lo <- swap_ms - window_s * 1000
  hi <- swap_ms + window_s * 1000
  df_win <- df[df$bucket >= lo & df$bucket <= hi, ]

  buckets <- sort(unique(df_win$bucket))

  cat(sprintf("\n── %s ── swap at %.1fs ──\n", toupper(label), swap_ms / 1000))
  cat(sprintf("%-12s %5s %6s  %8s %8s %8s %8s %8s\n",
              "time", "reqs", "err%", "p50", "avg", "IQR", "p99", "p999"))
  cat(sprintf("%-12s %5s %6s  %8s %8s %8s %8s %8s\n",
              "----------", "-----", "------", "--------", "--------",
              "--------", "--------", "--------"))

  for (b in buckets) {
    rows <- df_win[df_win$bucket == b, ]
    n <- nrow(rows)
    if (n == 0) next

    errs <- sum(rows$error != "none")
    err_pct <- errs / n * 100
    lat <- rows$duration_us

    p50  <- pct(lat, 0.50)
    avg  <- mean(lat)
    q1   <- pct(lat, 0.25)
    q3   <- pct(lat, 0.75)
    iqr  <- q3 - q1
    p99  <- pct(lat, 0.99)
    p999 <- pct(lat, 0.999)

    t_start <- b / 1000
    t_end   <- (b + 100) / 1000
    marker  <- ifelse(b <= swap_ms & swap_ms < b + 100, " <-SWAP", "")

    cat(sprintf("%5.1f-%5.1fs %5d %5.1f%%  %7.0fus %7.0fus %7.0fus %7.0fus %7.0fus%s\n",
                t_start, t_end, n, err_pct,
                p50, avg, iqr, p99, p999, marker))
  }

  # Aggregate summary
  cat(sprintf("\n  Total: %d requests, %.2f%% success, %.1f actual QPS\n",
              nrow(df),
              sum(df$error == "none") / nrow(df) * 100,
              nrow(df) / (max(df$start_ms) - min(df$start_ms)) * 1000))
  n_err <- sum(df$error != "none")
  if (n_err == 0) {
    cat("  Errors: 0\n")
  } else {
    err_tbl <- table(df$error[df$error != "none"])
    cat(sprintf("  Errors: %d (%s)\n", n_err,
                paste(names(err_tbl), err_tbl, sep = "=", collapse = ", ")))
  }
}

for (path in args) {
  bench <- read_bench(path)
  analyze_one(bench)
}
