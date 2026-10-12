const vscode = require('vscode');
exports.activate = async () => { await vscode.commands.executeCommand('workbench.view.extension.hupi'); };
