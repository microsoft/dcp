const fs = require('node:fs');

const header = `NOTICES AND INFORMATION
Do Not Translate or Localize

This software incorporates material from third parties.
Microsoft makes certain open source code available at https://3rdpartysource.microsoft.com,
or you may send a check or money order for US $5.00, including the product name,
the open source component name, platform, and version number, to:

Source Code Compliance Team
Microsoft Corporation
One Microsoft Way
Redmond, WA 98052
USA

Notwithstanding any other terms, you may reverse engineer this software to the extent
required to debug changes to any libraries licensed under the GNU Lesser General Public License.`;

const separator = '\n----------------------------------------------------------\n';

const licenseBlock = /BEGIN_LICENSE (?<name>.*@.*)\s(?<license>[\s\S]*)\sEND_LICENSE \1/gm;

fs.readdir('.', (err, files) => {
    if (err) throw err;

    const licenses = {};
    files.filter(file => file.startsWith('NOTICE.')).forEach(file => {
        const content = fs.readFileSync(file, 'utf8');

        // Iterate through all matches of the license block extracting the name and license and storing them in a dictionary with the name as the key and license block as the value.
        for (const match of content.matchAll(licenseBlock)) {
            licenses[match.groups.name] = match.groups.license;
        }
    });

    // Write the header and all licenses to a single NOTICE file with \n----------------------------------------------------------\n between each license block.
    const noticeContent = header + '\n' + separator + Object.values(licenses)
        .map(license => '\n' + license.trim() + '\n')
        .join(separator + separator);

    fs.writeFile('NOTICE', noticeContent, err => {
        if (err) throw err;
        console.log('NOTICE file has been created.');
    });
});