"""Logging configuration for the CLI agent."""

from __future__ import annotations

import logging
import os
from pathlib import Path


def setup_logging(log_file: str = None, log_level: str = "INFO") -> logging.Logger:
    """
    Configure logging for the CLI agent.
    
    Args:
        log_file: Path to log file. Defaults to cli/logs/agent.log
        log_level: Logging level (DEBUG, INFO, WARNING, ERROR)
    
    Returns:
        Configured logger instance
    """
    # Determine log file path
    if log_file is None:
        log_dir = Path(__file__).parent.parent / "logs"
        log_dir.mkdir(exist_ok=True)
        log_file = log_dir / "agent.log"
    else:
        log_file = Path(log_file)
        log_file.parent.mkdir(parents=True, exist_ok=True)

    # Get numeric log level
    numeric_level = getattr(logging, log_level.upper(), logging.INFO)

    # Create logger
    logger = logging.getLogger("cli_agent")
    logger.setLevel(numeric_level)

    # Avoid adding duplicate handlers
    if logger.handlers:
        return logger

    # File handler - detailed format
    file_handler = logging.FileHandler(log_file, encoding="utf-8")
    file_handler.setLevel(numeric_level)
    file_formatter = logging.Formatter(
        "%(asctime)s | %(levelname)-8s | %(name)s | %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )
    file_handler.setFormatter(file_formatter)
    logger.addHandler(file_handler)

    # Console handler - minimal format (only warnings and errors)
    console_handler = logging.StreamHandler()
    console_handler.setLevel(logging.WARNING)
    console_formatter = logging.Formatter("%(levelname)s: %(message)s")
    console_handler.setFormatter(console_formatter)
    logger.addHandler(console_handler)

    return logger


def get_logger(name: str = None) -> logging.Logger:
    """
    Get a child logger for a specific module.
    
    Args:
        name: Optional name suffix for the logger
    
    Returns:
        Logger instance
    """
    base_logger = logging.getLogger("cli_agent")
    if name:
        return base_logger.getChild(name)
    return base_logger
