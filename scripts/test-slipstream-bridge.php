<?php
declare(strict_types=1);

// Native smoke checks: php scripts/test-slipstream-bridge.php
// Fixtures live only in a newly created system temporary directory.

$bridge = __DIR__ . DIRECTORY_SEPARATOR . 'slipstream-archer.php';
$directory = sys_get_temp_dir() . DIRECTORY_SEPARATOR . 'archer-bridge-' . bin2hex(random_bytes(8));
if (!mkdir($directory, 0700)) {
    throw new RuntimeException('Cannot create the bridge fixture directory.');
}
$fixture = $directory . DIRECTORY_SEPARATOR . 'child.php';
$nonExecutable = $directory . DIRECTORY_SEPARATOR . 'non-executable';
file_put_contents($fixture, <<<'PHP'
<?php
fwrite(STDOUT, json_encode(['args' => array_slice($argv, 1), 'stdin' => stream_get_contents(STDIN)], JSON_THROW_ON_ERROR));
fwrite(STDERR, "fixture stderr\n");
exit(2);
PHP);

function check(bool $condition, string $description): void
{
    if (!$condition) {
        throw new RuntimeException($description);
    }
    fwrite(STDOUT, "PASS {$description}\n");
}

/** @return array{int, string, string} */
function runBridge(?string $binary, array $arguments = [], string $input = ''): array
{
    global $bridge;
    $environment = getenv();
    unset($environment['ARCHER_BINARY']);
    if ($binary !== null) {
        $environment['ARCHER_BINARY'] = $binary;
    }
    $process = proc_open(
        [PHP_BINARY, $bridge, ...$arguments],
        [0 => ['pipe', 'r'], 1 => ['pipe', 'w'], 2 => ['pipe', 'w']],
        $pipes,
        null,
        $environment,
        ['bypass_shell' => true],
    );
    if (!is_resource($process)) {
        throw new RuntimeException('Cannot start the bridge smoke fixture.');
    }
    fwrite($pipes[0], $input);
    fclose($pipes[0]);
    $output = stream_get_contents($pipes[1]);
    fclose($pipes[1]);
    $error = stream_get_contents($pipes[2]);
    fclose($pipes[2]);
    return [proc_close($process), $output, $error];
}

try {
    $literalArguments = ['with spaces', 'quote"and\'quote', '$HOME; & | $(echo injected)', 'Unicode: café'];
    $input = "{\"protocol\":\"archer.worker/v1\",\"text\":\"literal $ and &\"}\n";
    [$status, $output, $error] = runBridge(PHP_BINARY, [$fixture, ...$literalArguments], $input);
    $response = json_decode($output, true, 512, JSON_THROW_ON_ERROR);
    check($response['args'] === $literalArguments, 'argument boundaries and shell metacharacters remain literal');
    check($response['stdin'] === $input, 'JSON stdin is forwarded byte for byte');
    check($error === "fixture stderr\n", 'stderr passes through');
    check($status === 2, 'child exit status 2 is preserved');

    foreach ([null, '', 'relative-archer', $directory, $directory . DIRECTORY_SEPARATOR . 'missing'] as $invalid) {
        [$status, $output, $error] = runBridge($invalid);
        check($status === 1 && $output === '' && str_contains($error, 'Archer bridge:'), 'invalid binary configuration is rejected: ' . ($invalid ?? 'unset'));
    }
    if (PHP_OS_FAMILY !== 'Windows') {
        file_put_contents($nonExecutable, 'fixture');
        chmod($nonExecutable, 0600);
        [$status, $output, $error] = runBridge($nonExecutable);
        check($status === 1 && $output === '' && str_contains($error, 'not executable'), 'non-executable file is rejected');
    }
} finally {
    if (is_file($nonExecutable)) {
        unlink($nonExecutable);
    }
    unlink($fixture);
    rmdir($directory);
}
