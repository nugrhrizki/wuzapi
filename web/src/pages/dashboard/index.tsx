import { Box, Button, Flex, Text } from "@radix-ui/themes";

export default function Dashboard() {
  return (
    <Box>
      <Flex direction="column" gap="2">
        <Text>Hello from Radix Themes :)</Text>
      </Flex>
      <Button variant="classic">Let's go</Button>
    </Box>
  );
}
