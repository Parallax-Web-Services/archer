<?php
declare(strict_types=1);

/**
 * Bridge Slipstream's PHP_BINARY-prefixed ArcherProvider to the Archer executable.
 * Set ArcherProvider's binary to this file and ARCHER_BINARY to the absolute path
 * of an installed Archer executable. Arguments and standard streams pass through;
 * the child exit status, including 2 for unresolved requirements, is preserved.
 * PHP 8.4 compatible. No package installation or shell command construction.
 */

function fail(string $message): never
{
    fwrite(STDERR, "Archer bridge: {$message}\n");
    exit(1);
}

$binary = getenv('ARCHER_BINARY');
if ($binary === false || $binary === '') {
    fail('ARCHER_BINARY must name an absolute local Archer executable.');
}

$normalizedPath = str_replace('\\', '/', $binary);
$absolute = PHP_OS_FAMILY === 'Windows'
    ? preg_match('~^(?:[A-Za-z]:/|//[^/]+/[^/]+/)~', $normalizedPath) === 1
    : str_starts_with($binary, '/');
if (!$absolute || str_contains($binary, "\0") || !is_file($binary)) {
    fail('ARCHER_BINARY must name an existing absolute local executable file.');
}
if (!is_executable($binary)) {
    fail('ARCHER_BINARY is not executable.');
}

$command = [$binary, ...array_slice($argv, 1)];
$process = proc_open(
    $command,
    [0 => STDIN, 1 => STDOUT, 2 => STDERR],
    $pipes,
    null,
    null,
    ['bypass_shell' => true],
);
if (!is_resource($process)) {
    fail('Unable to launch ARCHER_BINARY.');
}

$status = proc_close($process);
if ($status < 0) {
    fail('Unable to determine Archer exit status.');
}
exit($status);
