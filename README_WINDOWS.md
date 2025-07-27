# Windows Terminal Usage Guide

## ✅ Fixed Console Issues

The terminal UI system now provides a much better experience on Windows:

### 🎯 **Recommended Usage for Windows:**

1. **Start with clean output:**
   ```bash
   ./main.exe
   ```

2. **Set appropriate log level for Windows:**
   ```bash
   loglevel WARN    # Only show warnings and errors (minimal interruption)
   # or
   loglevel INFO    # Show important events (default, some interruption)
   ```

3. **Use buffered logging for DEBUG/TRACE:**
   ```bash
   loglevel DEBUG   # Buffer detailed logs
   flush            # View buffered logs when needed
   ```

### 🛠️ **How the Windows Experience is Improved:**

**✅ Reduced Console Interruption:**
- `DEBUG` and `TRACE` messages are buffered instead of printed immediately
- Only `ERROR`, `WARN`, and `INFO` messages print immediately  
- Use `flush` command to view buffered detail when needed

**✅ Smart Log Level Defaults:**
- Default is `INFO` level (not `TRACE`)
- Most API spam is at `TRACE` level and won't show
- Important events still show in real-time

**✅ New Commands:**
- `flush` - View buffered logs without interruption
- `loglevel WARN` - Minimal output mode
- `help` - Fixed and simplified

### 📋 **Recommended Workflow:**

```bash
# 1. Start server with minimal noise
./main.exe
loglevel WARN

# 2. Use normally with minimal interruption
restart 0 5
status

# 3. Check details when needed  
flush
loglevel DEBUG
flush
loglevel WARN  # Back to quiet mode
```

### 🎮 **Best Settings for Different Scenarios:**

**Production/Normal Use:**
```bash
loglevel WARN     # Quiet, only important issues
```

**Development/Debugging:**
```bash  
loglevel DEBUG    # Buffer details, use 'flush' to view
```

**Troubleshooting:**
```bash
loglevel TRACE    # See everything (expect interruption)
```

This provides a much more manageable console experience on Windows while still allowing access to all the logging detail when needed.